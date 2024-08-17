package pods

import (
	"context"

	"log"
	"os"
	"os/exec"
	"time"

	"github.com/seneralkan/kubemon/internal/kubeclient"
	"github.com/seneralkan/kubemon/pkg/printer"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PodStatusMap struct {
	RunningPods   map[string]string
	PendingPods   map[string]string
	FailedPods    map[string]string
	SucceededPods map[string]string
	UnknownPods   map[string]string
}

type KubemonPods struct {
	KubeClientSet *kubeclient.KubeClientSet
	Namespace     string
	Label         string
}

func NewKubemonPods(k *kubeclient.KubeClientSet, namespace, label string) *KubemonPods {
	return &KubemonPods{
		KubeClientSet: k,
		Namespace:     namespace,
		Label:         label,
	}
}

func (k *KubemonPods) GetPods() (PodStatusMap, error) {
	clientSet, err := k.KubeClientSet.GetKubeClient()
	if err != nil {
		os.Exit(1)
	}

	pods, err := clientSet.CoreV1().Pods(k.Namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: k.Label,
	})

	if err != nil {
		log.Fatalf("Error getting pods: %s", err.Error())
		os.Exit(1)
	}

	if len(pods.Items) == 0 {
		log.Printf("No pods found with label: %s", k.Label)
		os.Exit(1)
	}

	runningPods := make(map[string]string)
	pendingPods := make(map[string]string)
	failedPods := make(map[string]string)
	succeededPods := make(map[string]string)
	unknownPods := make(map[string]string)

	for _, pod := range pods.Items {
		switch pod.Status.Phase {
		case "Running":
			runningPods[pod.Name] = string(pod.Status.Phase)
		case "Pending":
			pendingPods[pod.Name] = string(pod.Status.Phase)
		case "Failed":
			failedPods[pod.Name] = string(pod.Status.Phase)
		case "Succeeded":
			succeededPods[pod.Name] = string(pod.Status.Phase)
		default:
			unknownPods[pod.Name] = string(pod.Status.Phase)
		}
	}

	return PodStatusMap{
		RunningPods:   runningPods,
		PendingPods:   pendingPods,
		FailedPods:    failedPods,
		SucceededPods: succeededPods,
		UnknownPods:   unknownPods,
	}, nil
}

func (k *KubemonPods) Watch() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		clearTerminal()
		pods, err := k.GetPods()
		if err != nil {
			log.Printf("Error watching pods: %s", err.Error())
			continue
		}
		printer.PodsToTable("Running Pods ✅", pods.RunningPods)
		printer.PodsToTable("Pending Pods 🎯", pods.PendingPods)
		printer.PodsToTable("Failed Pods 😰", pods.FailedPods)
		printer.PodsToTable("Succeeded Pods ⚡️", pods.SucceededPods)
		printer.PodsToTable("Unknown Pods ❓", pods.UnknownPods)
	}
}

func clearTerminal() {
	cmd := exec.Command("clear")
	cmd.Stdout = os.Stdout
	cmd.Run()
}
