package main

import (
	"log"
	"os"
	"context"
	// "flag"

	"k8s.io/client-go/rest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	// "k8s.io/client-go/tools/clientcmd"
)

func GetCollocatedContainers() map[string]float64 {
	// kubeconfig := flag.String("kubeconfig", "/home/app/function/config", "location to kubeconfig file")
	// flag.Parse()

	// config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	// if err != nil {
	// 	panic(err.Error())

	// }

	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	// log.Printf("config: %v", config)
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	// log.Printf("clientset: %v", clientset)

	hostName, err := os.Hostname()
	if err != nil {
		panic(err.Error())
	}
	log.Printf(hostName)
	curPod, _ := clientset.CoreV1().Pods("openfaas-fn").List(context.Background(), metav1.ListOptions{FieldSelector: "metadata.name="+hostName})
	log.Printf("There are %d pods \n", len(curPod.Items))
	nodeName := curPod.Items[0].Spec.NodeName
    pods, err := clientset.CoreV1().Pods("openfaas-fn").List(context.Background(), metav1.ListOptions{FieldSelector: "spec.nodeName="+nodeName})
	// pods, err := clientset.CoreV1().Pods("openfaas").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}

	var collocatedContainers map[string]float64
	collocatedContainers = make(map[string]float64)
	var appName string
	for _, podInfo := range (*pods).Items {
		// fmt.Printf("pods-name=%v\n", podInfo.Labels["faas_function"])
		appName = podInfo.Labels["faas_function"]
		container, ok := collocatedContainers [ appName ]
		if ok {
			collocatedContainers[appName] += 1.0
		} else {
			collocatedContainers[appName] = 1.0
		}
		log.Printf("pods-name=%v, value=%f", appName, container)
		// log.Printf("app-name= %v\n", podInfo.Labels["faas_function"])
		// fmt.Printf("pods-status=%v\n", podInfo.Status.Phase)
		// fmt.Printf("pods-condition=%v\n", podInfo.Status.Conditions)
	}

	log.Printf("There are %d pods in the cluster under openfaas-fn\n", len(pods.Items))

	return collocatedContainers
}