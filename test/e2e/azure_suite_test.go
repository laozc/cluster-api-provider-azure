// +build e2e

/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e_test

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/config"
	"github.com/onsi/ginkgo/reporters"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	bootstrapv1 "sigs.k8s.io/cluster-api-bootstrap-provider-kubeadm/api/v1alpha2"
	infrav1 "sigs.k8s.io/cluster-api-provider-azure/api/v1alpha2"
	"sigs.k8s.io/cluster-api-provider-azure/test/e2e/auth"
	"sigs.k8s.io/cluster-api-provider-azure/test/e2e/framework"
	"sigs.k8s.io/cluster-api-provider-azure/test/e2e/framework/management/kind"
	"sigs.k8s.io/cluster-api-provider-azure/test/e2e/generators"
	capiv1 "sigs.k8s.io/cluster-api/api/v1alpha2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestE2E(t *testing.T) {
	artifactPath, _ := os.LookupEnv("ARTIFACTS")
	junitXML := fmt.Sprintf("junit.e2e_suite.%d.xml", config.GinkgoConfig.ParallelNode)
	junitPath := path.Join(artifactPath, junitXML)
	junitReporter := reporters.NewJUnitReporter(junitPath)

	RegisterFailHandler(Fail)
	RunSpecsWithDefaultAndCustomReporters(t, "CAPZ e2e suite", []Reporter{junitReporter})
}

var (
	ctx   = context.Background()
	creds auth.Creds
	mgmt  *kind.Cluster

	// TODO Parameterize some of these variables
	location       = "westus2"
	vmSize         = "Standard_B2ms"
	namespace      = "default"
	k8sVersion     = "v1.16.2"
	imageOffer     = "capi"
	imagePublisher = "cncf-upstream"
	imageSKU       = "k8s-1dot16-ubuntu-1804"
	imageVersion   = "latest"
)

var _ = BeforeSuite(func() {
	var err error

	By("Loading Azure credentials")
	if credsFile, found := os.LookupEnv("AZURE_CREDENTIALS"); found {
		creds, err = auth.LoadFromFile(credsFile)
	} else {
		creds, err = auth.LoadFromEnvironment()
	}
	Expect(err).NotTo(HaveOccurred())
	Expect(creds).NotTo(BeNil())
	Expect(creds.TenantID).NotTo(BeEmpty())
	Expect(creds.SubscriptionID).NotTo(BeEmpty())
	Expect(creds.ClientID).NotTo(BeEmpty())
	Expect(creds.ClientSecret).NotTo(BeEmpty())

	By("Creating management cluster")
	scheme := runtime.NewScheme()
	Expect(appsv1.AddToScheme(scheme)).To(Succeed())
	Expect(corev1.AddToScheme(scheme)).To(Succeed())
	Expect(capiv1.AddToScheme(scheme)).To(Succeed())
	Expect(bootstrapv1.AddToScheme(scheme)).To(Succeed())
	Expect(infrav1.AddToScheme(scheme)).To(Succeed())

	managerImage, found := os.LookupEnv("MANAGER_IMAGE")
	Expect(found).To(BeTrue(), fmt.Sprint("MANAGER_IMAGE not set"))

	mgmt, err = kind.NewCluster(ctx, "mgmt", scheme, managerImage)
	Expect(err).NotTo(HaveOccurred())
	Expect(mgmt).NotTo(BeNil())

	// TODO Figure out how to keep these versions in sync across the code base
	capi := &generators.ClusterAPI{Version: "v0.2.7"}
	cabpk := &generators.Bootstrap{Version: "v0.1.5"}
	infra := &generators.Infra{Creds: creds}

	framework.InstallComponents(ctx, mgmt, capi, cabpk, infra)

	// DO NOT stream "capi-controller-manager" logs as it prints out azure.json
	// go func() {
	// 	defer GinkgoRecover()
	// 	watchDeployment(mgmt, "cabpk-system", "cabpk-controller-manager")
	// }()
	// go func() {
	// 	defer GinkgoRecover()
	// 	watchDeployment(mgmt, "capz-system", "capz-controller-manager")
	// }()
})

var _ = AfterSuite(func() {
	By("Tearing down management cluster")
	Expect(mgmt.Teardown(ctx)).NotTo(HaveOccurred())
})

func watchDeployment(mgmt *kind.Cluster, namespace, name string) {
	artifactPath, _ := os.LookupEnv("ARTIFACTS")
	logDir := path.Join(artifactPath, "logs")

	c, err := mgmt.GetClient()
	Expect(err).NotTo(HaveOccurred())

	waitDeployment(c, namespace, name)

	deployment := &appsv1.Deployment{}
	deploymentKey := client.ObjectKey{Namespace: namespace, Name: name}
	Expect(c.Get(ctx, deploymentKey, deployment)).To(Succeed())

	selector, err := metav1.LabelSelectorAsMap(deployment.Spec.Selector)
	Expect(err).NotTo(HaveOccurred())

	pods := &corev1.PodList{}
	Expect(c.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels(selector))).To(Succeed())

	for _, pod := range pods.Items {
		for _, container := range deployment.Spec.Template.Spec.Containers {
			if container.Name != "manager" {
				continue
			}
			logFile := path.Join(logDir, name, pod.Name, container.Name+".log")
			Expect(os.MkdirAll(filepath.Dir(logFile), 0755)).To(Succeed())

			clientSet, err := mgmt.Clientset()
			Expect(err).NotTo(HaveOccurred())

			opts := &corev1.PodLogOptions{Container: container.Name, Follow: true}
			logsStream, err := clientSet.CoreV1().Pods(namespace).GetLogs(pod.Name, opts).Stream()
			Expect(err).NotTo(HaveOccurred())
			defer logsStream.Close()

			f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			Expect(err).NotTo(HaveOccurred())
			defer f.Close()

			out := bufio.NewWriter(f)
			defer out.Flush()

			_, err = out.ReadFrom(logsStream)
			if err != nil && err.Error() != "unexpected EOF" {
				Expect(err).NotTo(HaveOccurred())
			}
		}
	}
}

func waitDeployment(c client.Client, namespace, name string) {
	Eventually(func() (int32, error) {
		deployment := &appsv1.Deployment{}
		deploymentKey := client.ObjectKey{Namespace: namespace, Name: name}
		if err := c.Get(context.TODO(), deploymentKey, deployment); err != nil {
			return 0, err
		}
		return deployment.Status.ReadyReplicas, nil
	}, 5*time.Minute, 15*time.Second,
		fmt.Sprintf("Deployment %s/%s could not reach the ready state", namespace, name),
	).ShouldNot(BeZero())
}
