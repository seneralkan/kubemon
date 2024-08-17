/*
Copyright © 2024 SENER ALKAN
*/
package cmd

import (
	"fmt"

	"github.com/seneralkan/kubemon/internal/kubeclient"
	"github.com/seneralkan/kubemon/internal/pods"

	"github.com/spf13/cobra"
)

// podsCmd represents the pods command
var podsCmd = &cobra.Command{
	Use:   "pods [--namespace <namespace>] [--label <label>]",
	Short: "Watch the status of pods in a kubernetes cluster based on the namespace and label",
	Long: `Watch the status of pods in a kubernetes cluster based on the namespace and label.
	You can specify the namespace and label to filter the pods.
	For example:
	kubemon pods --namespace foo --label app=nginx`,
	Run: func(cmd *cobra.Command, args []string) {
		namespace := cmd.Flag("namespace").Value.String()
		label := cmd.Flag("label").Value.String()
		fmt.Println("Namespace ☄️", namespace)
		fmt.Println("Label 🏷️", label)

		kubeClientSet := kubeclient.NewKubeClientSet()
		kubePods := pods.NewKubemonPods(kubeClientSet, namespace, label)
		kubePods.Watch()
	},
}

func init() {
	rootCmd.AddCommand(podsCmd)

	// Here you will define your flags and configuration settings.
	// Cobra supports Persistent Flags which will work for this command
	podsCmd.PersistentFlags().String("namespace", "default", "A help for defining the namespace")
	podsCmd.PersistentFlags().String("label", "", "A help for defining the app label. Default is none")
}
