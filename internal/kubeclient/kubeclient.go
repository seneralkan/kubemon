package kubeclient

import (
	"log"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TODO - Implement optinal flags for kubeconfig and context
type KubeClientSet struct {
	homeDir string
}

func NewKubeClientSet() *KubeClientSet {
	return &KubeClientSet{
		homeDir: getHomeDir(),
	}
}

func (k *KubeClientSet) GetKubeClient() (*kubernetes.Clientset, error) {

	kubeConfig := filepath.Join(k.homeDir, ".kube", "config")
	config, err := clientcmd.BuildConfigFromFlags("", kubeConfig)

	if err != nil {
		log.Fatalf("Error building kubeconfig: %s", err.Error())
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)

	if err != nil {
		log.Fatalf("Error building kubernetes clientset: %s", err.Error())
		return nil, err
	}

	return clientset, nil
}

func getHomeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	// windows
	return os.Getenv("USERPROFILE")
}
