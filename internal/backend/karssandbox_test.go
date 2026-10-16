package backend

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestKarsProxyIngress(t *testing.T) {
	client := fake.NewClientset()
	k := &KarsSandboxBackend{k8s: client, namespace: "agents", port: 9222}
	for range 2 {
		if err := k.ensureProxyIngress(t.Context(), "kars-pw-alpha", "pw-alpha"); err != nil {
			t.Fatal(err)
		}
	}
	p, err := client.NetworkingV1().NetworkPolicies("kars-pw-alpha").Get(t.Context(), "playwright-proxy-ingress", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	peer := p.Spec.Ingress[0].From[0]
	if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "agents" || peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "playwright-proxy" {
		t.Fatalf("ingress too broad: %v", peer)
	}
	if p.Spec.Ingress[0].Ports[0].Port.IntVal != 9222 || len(p.Spec.Egress) != 0 {
		t.Fatalf("unexpected traffic rules: %v", p.Spec)
	}
	p.Spec.Ingress[0].Ports[0].Port.IntVal = 8443
	if _, err := client.NetworkingV1().NetworkPolicies(p.Namespace).Update(t.Context(), p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := k.ensureProxyIngress(t.Context(), p.Namespace, "pw-alpha"); err == nil {
		t.Fatal("conflicting policy accepted")
	}
}

func TestKarsRequiresReadyPod(t *testing.T) {
	pending := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "not-ready", Namespace: "sandbox", Labels: map[string]string{"kars.azure.com/component": "sandbox"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.2"}}
	ready := pending.DeepCopy()
	ready.Name = "ready"
	ready.Status.PodIP = "10.0.0.3"
	ready.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	k := &KarsSandboxBackend{k8s: fake.NewClientset(pending, ready)}
	if ip, err := k.waitForPodIP(t.Context(), "sandbox"); err != nil || ip != ready.Status.PodIP {
		t.Fatalf("ip=%s error=%v", ip, err)
	}
}
