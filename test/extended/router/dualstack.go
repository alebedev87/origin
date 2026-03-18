package router

import (
	"context"
	"fmt"
	"strings"
	"time"

	g "github.com/onsi/ginkgo/v2"
	o "github.com/onsi/gomega"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	routev1 "github.com/openshift/api/route/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	e2e "k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eoutput "k8s.io/kubernetes/test/e2e/framework/pod/output"
	admissionapi "k8s.io/pod-security-admission/api"
	utilpointer "k8s.io/utils/pointer"

	"github.com/openshift/origin/test/extended/router/shard"
	exutil "github.com/openshift/origin/test/extended/util"
	"github.com/openshift/origin/test/extended/util/image"
)

var _ = g.Describe("[sig-network-edge][OCPFeatureGate:AWSDualStackInstall][Feature:Router][apigroup:route.openshift.io][apigroup:operator.openshift.io][apigroup:config.openshift.io]", func() {
	defer g.GinkgoRecover()

	var oc = exutil.NewCLIWithPodSecurityLevel("router-dualstack", admissionapi.LevelBaseline)

	g.It("should be reachable via IPv4 and IPv6 through a dual-stack ingress controller", func() {
		ctx := context.Background()

		g.By("Checking that the Infrastructure CR has a DualStack IPFamily")
		infra, err := oc.AdminConfigClient().ConfigV1().Infrastructures().Get(ctx, "cluster", metav1.GetOptions{})
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to get infrastructure CR")

		if infra.Status.PlatformStatus == nil || infra.Status.PlatformStatus.Type != configv1.AWSPlatformType {
			g.Skip("Test requires AWS platform")
		}
		if infra.Status.PlatformStatus.AWS == nil {
			g.Skip("AWS platform status is not set")
		}
		ipFamily := infra.Status.PlatformStatus.AWS.IPFamily
		if ipFamily != configv1.DualStackIPv4Primary && ipFamily != configv1.DualStackIPv6Primary {
			g.Skip(fmt.Sprintf("Test requires DualStack IPFamily, got %q", ipFamily))
		}

		g.By("Getting the default ingress domain")
		defaultDomain, err := getDefaultIngressClusterDomainName(oc, time.Minute)
		o.Expect(err).NotTo(o.HaveOccurred(), "failed to find default domain name")

		ns := oc.KubeFramework().Namespace.Name
		baseDomain := strings.TrimPrefix(defaultDomain, "apps.")
		shardFQDN := "hosts." + baseDomain

		// Deploy the shard first so DNS and LB can provision while we set up the backend.
		g.By("Deploying a new router shard with NLB")
		shardIngressCtrl, err := shard.DeployNewRouterShard(oc, 10*time.Minute, shard.Config{
			Domain: shardFQDN,
			Type:   oc.Namespace(),
			LoadBalancer: &operatorv1.LoadBalancerStrategy{
				Scope: operatorv1.ExternalLoadBalancer,
				ProviderParameters: &operatorv1.ProviderLoadBalancerParameters{
					Type: operatorv1.AWSLoadBalancerProvider,
					AWS: &operatorv1.AWSLoadBalancerParameters{
						Type: operatorv1.AWSNetworkLoadBalancer,
					},
				},
			},
		})
		defer func() {
			if shardIngressCtrl != nil {
				if err := oc.AdminOperatorClient().OperatorV1().IngressControllers(shardIngressCtrl.Namespace).Delete(ctx, shardIngressCtrl.Name, metav1.DeleteOptions{}); err != nil {
					e2e.Logf("deleting ingress controller failed: %v\n", err)
				}
			}
		}()
		o.Expect(err).NotTo(o.HaveOccurred(), "new router shard did not rollout")

		g.By("Labelling the namespace for the shard")
		err = oc.AsAdmin().Run("label").Args("namespace", oc.Namespace(), "type="+oc.Namespace()).Execute()
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Creating backend service")
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: "dualstack-backend",
				Labels: map[string]string{
					"app": "dualstack-backend",
				},
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{
					"app": "dualstack-backend",
				},
				IPFamilyPolicy: func() *corev1.IPFamilyPolicy {
					p := corev1.IPFamilyPolicyPreferDualStack
					return &p
				}(),
				Ports: []corev1.ServicePort{
					{
						Name:       "http",
						Port:       8080,
						Protocol:   corev1.ProtocolTCP,
						TargetPort: intstr.FromInt(8080),
					},
				},
			},
		}
		_, err = oc.AdminKubeClient().CoreV1().Services(ns).Create(ctx, service, metav1.CreateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Creating backend pod")
		backendPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "dualstack-backend",
				Labels: map[string]string{
					"app": "dualstack-backend",
				},
			},
			Spec: corev1.PodSpec{
				TerminationGracePeriodSeconds: utilpointer.Int64(1),
				Containers: []corev1.Container{
					{
						Name:            "server",
						Image:           image.ShellImage(),
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"/bin/bash", "-c", `while true; do
printf "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nContent-Type: text/plain\r\n\r\nOK" | ncat -l 8080 --send-only || true
done`},
						Ports: []corev1.ContainerPort{
							{
								ContainerPort: 8080,
								Name:          "http",
								Protocol:      corev1.ProtocolTCP,
							},
						},
					},
				},
			},
		}
		_, err = oc.AdminKubeClient().CoreV1().Pods(ns).Create(ctx, backendPod, metav1.CreateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Waiting for backend pod to be running")
		e2e.ExpectNoError(e2epod.WaitForPodRunningInNamespaceSlow(ctx, oc.KubeClient(), "dualstack-backend", ns), "backend pod not running")

		g.By("Creating an edge-terminated route")
		routeType := oc.Namespace()
		route := routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name: "dualstack-route",
				Labels: map[string]string{
					"type": routeType,
				},
			},
			Spec: routev1.RouteSpec{
				Host: "dualstack-test." + shardFQDN,
				Port: &routev1.RoutePort{
					TargetPort: intstr.FromInt(8080),
				},
				TLS: &routev1.TLSConfig{
					Termination:                   routev1.TLSTerminationEdge,
					InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
				},
				To: routev1.RouteTargetReference{
					Kind:   "Service",
					Name:   "dualstack-backend",
					Weight: utilpointer.Int32(100),
				},
				WildcardPolicy: routev1.WildcardPolicyNone,
			},
		}
		_, err = oc.RouteClient().RouteV1().Routes(ns).Create(ctx, &route, metav1.CreateOptions{})
		o.Expect(err).NotTo(o.HaveOccurred())

		g.By("Waiting for the route to be admitted")
		routeHost := "dualstack-test." + shardFQDN
		err = wait.PollImmediate(5*time.Second, 5*time.Minute, func() (bool, error) {
			r, err := oc.RouteClient().RouteV1().Routes(ns).Get(ctx, "dualstack-route", metav1.GetOptions{})
			if err != nil {
				e2e.Logf("failed to get route: %v, retrying...", err)
				return false, nil
			}
			for _, ingress := range r.Status.Ingress {
				if ingress.Host == routeHost {
					for _, condition := range ingress.Conditions {
						if condition.Type == routev1.RouteAdmitted && condition.Status == corev1.ConditionTrue {
							return true, nil
						}
					}
				}
			}
			return false, nil
		})
		o.Expect(err).NotTo(o.HaveOccurred(), "route was not admitted")

		g.By("Creating exec pod for curl tests")
		execPod := exutil.CreateExecPodOrFail(oc.AdminKubeClient(), ns, "execpod")
		defer func() {
			oc.AdminKubeClient().CoreV1().Pods(ns).Delete(ctx, execPod.Name, *metav1.NewDeleteOptions(1))
		}()

		g.By("Verifying route is reachable over IPv4")
		err = waitForDualStackRouteResponse(ns, execPod.Name, routeHost, "-4", 10*time.Minute)
		o.Expect(err).NotTo(o.HaveOccurred(), "route not reachable over IPv4")

		g.By("Verifying route is reachable over IPv6")
		err = waitForDualStackRouteResponse(ns, execPod.Name, routeHost, "-6", 10*time.Minute)
		o.Expect(err).NotTo(o.HaveOccurred(), "route not reachable over IPv6")
	})
})

func waitForDualStackRouteResponse(ns, execPodName, host, ipFlag string, timeout time.Duration) error {
	curlCmd := fmt.Sprintf("curl %s -k -v -m 10 --connect-timeout 5 -o /dev/null https://%s 2>&1", ipFlag, host)
	var lastOutput string
	err := wait.PollImmediate(5*time.Second, timeout, func() (bool, error) {
		output, err := e2eoutput.RunHostCmd(ns, execPodName, curlCmd)
		lastOutput = output
		e2e.Logf("curl %s %s:\n%s", ipFlag, host, output)
		if err != nil {
			e2e.Logf("curl %s error: %v", ipFlag, err)
			return false, nil
		}
		if strings.Contains(output, "< HTTP/1.1 200") || strings.Contains(output, "< HTTP/2 200") {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("curl %s to %s timed out, last output:\n%s", ipFlag, host, lastOutput)
	}
	return nil
}
