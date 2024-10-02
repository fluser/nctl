package auth

import (
	"context"
	"io"
	"log"
	"os"
	"testing"

	infrastructure "github.com/ninech/apis/infrastructure/v1alpha1"
	"github.com/ninech/nctl/api"
	"github.com/ninech/nctl/api/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const existingKubeconfig = `
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://existing.example.org
  name: existing
users:
- name: existing
current-context: existing
contexts:
- context:
  name: existing
`

func TestClusterCmd(t *testing.T) {
	// write our "existing" kubeconfig to a temp kubeconfig
	kubeconfig, err := os.CreateTemp("", "*-kubeconfig.yaml")
	if err != nil {
		log.Fatal(err)
	}
	defer os.Remove(kubeconfig.Name())

	if err := os.WriteFile(kubeconfig.Name(), []byte(existingKubeconfig), os.ModePerm); err != nil {
		t.Fatal(err)
	}

	// prepare a fake client with some static objects (a KubernetesCluster and
	// a Secret) that the cluster cmd expects.
	scheme, err := api.NewScheme()
	if err != nil {
		t.Fatal(err)
	}

	cluster := newCluster()
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	// we run without the execPlugin, that would be something for an e2e test
	cmd := &ClusterCmd{Name: config.ContextName(cluster), ExecPlugin: false}
	if err := cmd.Run(context.TODO(), &api.Client{WithWatch: client, KubeconfigPath: kubeconfig.Name()}); err != nil {
		t.Fatal(err)
	}

	// read out the kubeconfig again to test the contents
	b, err := io.ReadAll(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}

	merged, err := clientcmd.Load(b)
	if err != nil {
		t.Fatal(err)
	}

	checkConfig(t, merged, 2, config.ContextName(cluster))
}

func newCluster() *infrastructure.KubernetesCluster {
	return &infrastructure.KubernetesCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "test",
		},
		Spec: infrastructure.KubernetesClusterSpec{},
		Status: infrastructure.KubernetesClusterStatus{
			AtProvider: infrastructure.KubernetesClusterObservation{
				ClusterObservation: infrastructure.ClusterObservation{
					APIEndpoint:   "https://new.example.org",
					OIDCClientID:  "some-client-id",
					OIDCIssuerURL: "https://auth.example.org",
				},
			},
		},
	}
}
