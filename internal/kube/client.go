// Package kube builds Kubernetes clients the same way kubectl does
// (--kubeconfig, --context, -n, KUBECONFIG, in-cluster fallback).
package kube

import (
	"fmt"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Clients bundles the typed and dynamic clients plus the effective namespace.
type Clients struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	// Namespace is the --namespace flag, else the kubeconfig context namespace, else "default".
	Namespace string
}

// New builds clients from kubectl-style config flags.
func New(flags *genericclioptions.ConfigFlags) (*Clients, error) {
	cfg, err := flags.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	cfg.QPS, cfg.Burst = 50, 100

	ns, _, err := flags.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return nil, fmt.Errorf("resolve namespace: %w", err)
	}
	k, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	d, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	return &Clients{Kube: k, Dynamic: d, Namespace: ns}, nil
}
