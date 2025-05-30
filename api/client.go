package api

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"github.com/crossplane/crossplane-runtime/pkg/resource"
	"github.com/ninech/apis"
	infrastructure "github.com/ninech/apis/infrastructure/v1alpha1"
	meta "github.com/ninech/apis/meta/v1alpha1"
	"github.com/ninech/nctl/api/config"
	"github.com/ninech/nctl/api/log"
	"github.com/ninech/nctl/internal/format"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type Client struct {
	runtimeclient.WithWatch
	Config            *rest.Config
	KubeconfigPath    string
	Project           string
	Log               *log.Client
	KubeconfigContext string
}

type ClientOpt func(c *Client) error

// New returns a new Client by loading a kubeconfig with the supplied context
// and project. The kubeconfig is discovered like this:
// * KUBECONFIG environment variable pointing at a file
// * $HOME/.kube/config if exists
func New(ctx context.Context, apiClusterContext, project string, opts ...ClientOpt) (*Client, error) {
	client := &Client{
		Project:           project,
		KubeconfigContext: apiClusterContext,
	}
	if err := client.loadConfig(apiClusterContext); err != nil {
		return nil, err
	}

	scheme, err := NewScheme()
	if err != nil {
		return nil, err
	}

	c, err := runtimeclient.NewWithWatch(client.Config, runtimeclient.Options{
		Scheme: scheme,
	})
	if err != nil {
		return nil, err
	}
	client.WithWatch = c

	for _, opt := range opts {
		if err := opt(client); err != nil {
			return nil, err
		}
	}

	return client, nil
}

// LogClient sets up a log client connected to the provided address.
func LogClient(ctx context.Context, address string, insecure bool) ClientOpt {
	return func(c *Client) error {
		logClient, err := log.NewClient(address, func(ctx context.Context) string { return c.Token(ctx) }, c.Project, insecure)
		if err != nil {
			return fmt.Errorf("unable to create log client: %w", err)
		}
		c.Log = logClient
		return nil
	}
}

// StaticToken configures the client to get a bearer token once and then set it
// statically in the client config. This means the client will not automatically
// renew the token when it expires.
func StaticToken(ctx context.Context) ClientOpt {
	return func(c *Client) error {
		c.Config.BearerToken = c.Token(ctx)
		tokenClient, err := runtimeclient.NewWithWatch(c.Config, runtimeclient.Options{
			Scheme: c.Scheme(),
		})
		if err != nil {
			return err
		}
		c.WithWatch = tokenClient

		return nil
	}
}

// NewScheme returns a *runtime.Scheme with all the relevant types registered.
func NewScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := apis.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return scheme, nil
}

// adapted from https://github.com/kubernetes-sigs/controller-runtime/blob/4c9c9564e4652bbdec14a602d6196d8622500b51/pkg/client/config/config.go#L116
func (c *Client) loadConfig(context string) error {
	loadingRules, err := LoadingRules()
	if err != nil {
		return err
	}

	cfg, project, err := loadConfigWithContext("", loadingRules, context)
	if err != nil {
		return err
	}
	if c.Project == "" {
		c.Project = project
	}
	c.Config = cfg
	c.KubeconfigPath = loadingRules.GetDefaultFilename()

	return nil
}

func (c *Client) Name(name string) types.NamespacedName {
	return types.NamespacedName{Name: name, Namespace: c.Project}
}

func (c *Client) GetConnectionSecret(ctx context.Context, mg resource.Managed) (*corev1.Secret, error) {
	if mg.GetWriteConnectionSecretToReference() == nil {
		return nil, fmt.Errorf("%T %s/%s has no connection secret ref set", mg, mg.GetName(), mg.GetNamespace())
	}

	nsName := types.NamespacedName{
		Name:      mg.GetWriteConnectionSecretToReference().Name,
		Namespace: mg.GetWriteConnectionSecretToReference().Namespace,
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, nsName, secret); err != nil {
		return nil, fmt.Errorf("unable to get referenced secret %v: %w", nsName, err)
	}

	return secret, nil
}

func (c *Client) Token(ctx context.Context) string {
	if c.Config == nil {
		return ""
	}

	token, err := GetTokenFromConfig(ctx, c.Config)
	if err != nil {
		return ""
	}

	return token
}

func (c *Client) DeploioRuntimeClient(ctx context.Context, scheme *runtime.Scheme) (runtimeclient.Client, error) {
	cfg, err := c.DeploioRuntimeConfig(ctx)
	if err != nil {
		return nil, err
	}
	return runtimeclient.New(cfg, runtimeclient.Options{Scheme: scheme})
}

func (c *Client) DeploioRuntimeConfig(ctx context.Context) (*rest.Config, error) {
	config := rest.CopyConfig(c.Config)
	deploioClusterData := &infrastructure.ClusterData{}
	if err := c.Get(ctx, types.NamespacedName{Name: meta.ClusterDataDeploioName}, deploioClusterData); err != nil {
		return nil, fmt.Errorf("can not gather deplo.io cluster connection details: %w", err)
	}
	config.Host = deploioClusterData.Status.AtProvider.APIEndpoint
	var err error
	if config.CAData, err = base64.StdEncoding.DecodeString(deploioClusterData.Status.AtProvider.APICACert); err != nil {
		return nil, fmt.Errorf("can not decode deplo.io cluster CA certificate: %w", err)
	}
	return config, nil
}

func (c *Client) Organization() (string, error) {
	cfg, err := config.ReadExtension(c.KubeconfigPath, c.KubeconfigContext)
	if err != nil {
		if config.IsExtensionNotFoundError(err) {
			return "", reloginNeeded(err)
		}
		return "", err
	}

	return cfg.Organization, nil
}

// reloginNeeded returns an error which outputs the given err with a message
// saying that a re-login is needed.
func reloginNeeded(err error) error {
	return fmt.Errorf(
		"%w, please re-login by executing %q",
		err,
		format.Command().Login(),
	)
}

func LoadingRules() (*clientcmd.ClientConfigLoadingRules, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if _, ok := os.LookupEnv("HOME"); !ok {
		u, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("could not get current user: %w", err)
		}
		loadingRules.Precedence = append(
			loadingRules.Precedence,
			filepath.Join(u.HomeDir, clientcmd.RecommendedHomeDir, clientcmd.RecommendedFileName),
		)
	}

	return loadingRules, nil
}

func loadConfigWithContext(apiServerURL string, loader clientcmd.ClientConfigLoader, context string) (*rest.Config, string, error) {
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loader, &clientcmd.ConfigOverrides{
			ClusterInfo: clientcmdapi.Cluster{
				Server: apiServerURL,
			},
			CurrentContext: context,
		},
	)

	ns, _, err := clientConfig.Namespace()
	if err != nil {
		return nil, "", err
	}

	cfg, err := clientConfig.ClientConfig()
	cfg.QPS = 25
	cfg.Burst = 50
	return cfg, ns, err
}

func ObjectName(obj runtimeclient.Object) types.NamespacedName {
	return types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
}

func NamespacedName(name, project string) types.NamespacedName {
	return types.NamespacedName{Name: name, Namespace: project}
}
