/*
Copyright 2025.

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

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/google/go-github/v62/github"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	k8sptr "k8s.io/utils/ptr"

	kuberikv1alpha1 "github.com/kuberik/environment-controller/api/v1alpha1"
	kuberikrolloutv1alpha1 "github.com/kuberik/rollout-controller/api/v1alpha1"
)

var _ = Describe("Environment relationship types", func() {
	const namespace = "default"

	var reconciler *GitHubEnvironmentReconciler

	BeforeEach(func() {
		reconciler = &GitHubEnvironmentReconciler{
			Client: k8sClient,
			Scheme: scheme.Scheme,
		}
	})

	Context("relationshipAllowsBakeStatus", func() {
		DescribeTable("decides which related environment bake statuses pass the gate",
			func(relType kuberikv1alpha1.RelationshipType, bakeStatus *string, allowed bool) {
				Expect(relationshipAllowsBakeStatus(relType, bakeStatus)).To(Equal(allowed))
			},
			Entry("Alongside allows Deploying", kuberikv1alpha1.RelationshipTypeAlongside, k8sptr.To(kuberikrolloutv1alpha1.BakeStatusDeploying), true),
			Entry("Alongside allows InProgress", kuberikv1alpha1.RelationshipTypeAlongside, k8sptr.To(kuberikrolloutv1alpha1.BakeStatusInProgress), true),
			Entry("Alongside allows Succeeded", kuberikv1alpha1.RelationshipTypeAlongside, k8sptr.To(kuberikrolloutv1alpha1.BakeStatusSucceeded), true),
			Entry("Alongside excludes Failed", kuberikv1alpha1.RelationshipTypeAlongside, k8sptr.To(kuberikrolloutv1alpha1.BakeStatusFailed), false),
			Entry("Alongside excludes Cancelled", kuberikv1alpha1.RelationshipTypeAlongside, k8sptr.To(kuberikrolloutv1alpha1.BakeStatusCancelled), false),
			Entry("Alongside excludes a version without bake status", kuberikv1alpha1.RelationshipTypeAlongside, nil, false),
			Entry("After allows Succeeded", kuberikv1alpha1.RelationshipTypeAfter, k8sptr.To(kuberikrolloutv1alpha1.BakeStatusSucceeded), true),
			Entry("After excludes Deploying", kuberikv1alpha1.RelationshipTypeAfter, k8sptr.To(kuberikrolloutv1alpha1.BakeStatusDeploying), false),
			Entry("After excludes InProgress", kuberikv1alpha1.RelationshipTypeAfter, k8sptr.To(kuberikrolloutv1alpha1.BakeStatusInProgress), false),
			Entry("After excludes Failed", kuberikv1alpha1.RelationshipTypeAfter, k8sptr.To(kuberikrolloutv1alpha1.BakeStatusFailed), false),
			Entry("After excludes Cancelled", kuberikv1alpha1.RelationshipTypeAfter, k8sptr.To(kuberikrolloutv1alpha1.BakeStatusCancelled), false),
			Entry("After excludes a version without bake status", kuberikv1alpha1.RelationshipTypeAfter, nil, false),
			Entry("legacy Parallel behaves like After", kuberikv1alpha1.RelationshipType("Parallel"), k8sptr.To(kuberikrolloutv1alpha1.BakeStatusDeploying), false),
			Entry("legacy Parallel allows Succeeded", kuberikv1alpha1.RelationshipType("Parallel"), k8sptr.To(kuberikrolloutv1alpha1.BakeStatusSucceeded), true),
		)
	})

	Context("normalizeRelationship", func() {
		It("Should keep known types and the related environment", func() {
			for _, relType := range []kuberikv1alpha1.RelationshipType{kuberikv1alpha1.RelationshipTypeAfter, kuberikv1alpha1.RelationshipTypeAlongside} {
				rel := &kuberikv1alpha1.EnvironmentRelationship{Environment: "prod", Type: relType}
				Expect(normalizeRelationship(rel)).To(Equal(rel))
			}
		})

		It("Should treat unknown types as After without modifying the input", func() {
			for _, relType := range []kuberikv1alpha1.RelationshipType{"Parallel", "", "SomethingNew"} {
				rel := &kuberikv1alpha1.EnvironmentRelationship{Environment: "prod", Type: relType}
				normalized := normalizeRelationship(rel)
				Expect(normalized.Environment).To(Equal("prod"))
				Expect(normalized.Type).To(Equal(kuberikv1alpha1.RelationshipTypeAfter))
				Expect(rel.Type).To(Equal(relType))
			}
		})

		It("Should return nil for nil", func() {
			Expect(normalizeRelationship(nil)).To(BeNil())
		})

		It("Should normalize the relationship of a legacy deployment payload", func() {
			dep := &github.Deployment{
				Payload: json.RawMessage(`{"id":"1","relationship":{"environment":"prod","type":"Parallel"},"version":{"tag":"v1.0.0","revision":"abc"}}`),
			}
			payload := reconciler.extractDeploymentPayload(dep)
			Expect(payload).ToNot(BeNil())
			Expect(payload.Relationship).To(Equal(&kuberikv1alpha1.EnvironmentRelationship{
				Environment: "prod",
				Type:        kuberikv1alpha1.RelationshipTypeAfter,
			}))
		})

		It("Should keep an Alongside relationship in a deployment payload", func() {
			dep := &github.Deployment{
				Payload: json.RawMessage(`{"id":"1","relationship":{"environment":"prod","type":"Alongside"},"version":{"tag":"v1.0.0","revision":"abc"}}`),
			}
			payload := reconciler.extractDeploymentPayload(dep)
			Expect(payload).ToNot(BeNil())
			Expect(payload.Relationship.Type).To(Equal(kuberikv1alpha1.RelationshipTypeAlongside))
		})
	})

	Context("CRD validation", func() {
		newEnvironment := func(name string, relType kuberikv1alpha1.RelationshipType) *kuberikv1alpha1.Environment {
			return &kuberikv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: kuberikv1alpha1.EnvironmentSpec{
					RolloutRef:  corev1.LocalObjectReference{Name: "some-rollout"},
					Name:        "relationship-validation",
					Environment: "prod-eu-1",
					Relationship: &kuberikv1alpha1.EnvironmentRelationship{
						Environment: "prod",
						Type:        relType,
					},
					Backend: kuberikv1alpha1.BackendConfig{Type: "github", Project: "kuberik/environment-controller-testing"},
				},
			}
		}

		It("Should accept an Alongside relationship", func() {
			env := newEnvironment("relationship-validation-alongside", kuberikv1alpha1.RelationshipTypeAlongside)
			Expect(k8sClient.Create(context.Background(), env)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(context.Background(), env)).To(Succeed()) })
		})

		It("Should reject the removed Parallel relationship", func() {
			env := newEnvironment("relationship-validation-parallel", "Parallel")
			err := k8sClient.Create(context.Background(), env)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got %v", err)
			Expect(err.Error()).To(ContainSubstring("Alongside"))
		})
	})

	Context("updateAllowedVersionsFromRelationships", func() {
		// The leader's history holds one version per bake status, plus one it hasn't reported a status for yet
		leaderHistory := []kuberikrolloutv1alpha1.DeploymentHistoryEntry{
			{ID: k8sptr.To(int64(6)), Version: kuberikrolloutv1alpha1.VersionInfo{Tag: "v6", Revision: k8sptr.To("rev-6")}, Timestamp: metav1.Now()},
			{ID: k8sptr.To(int64(5)), Version: kuberikrolloutv1alpha1.VersionInfo{Tag: "v5", Revision: k8sptr.To("rev-5")}, Timestamp: metav1.Now(), BakeStatus: k8sptr.To(kuberikrolloutv1alpha1.BakeStatusDeploying)},
			{ID: k8sptr.To(int64(4)), Version: kuberikrolloutv1alpha1.VersionInfo{Tag: "v4", Revision: k8sptr.To("rev-4")}, Timestamp: metav1.Now(), BakeStatus: k8sptr.To(kuberikrolloutv1alpha1.BakeStatusCancelled)},
			{ID: k8sptr.To(int64(3)), Version: kuberikrolloutv1alpha1.VersionInfo{Tag: "v3", Revision: k8sptr.To("rev-3")}, Timestamp: metav1.Now(), BakeStatus: k8sptr.To(kuberikrolloutv1alpha1.BakeStatusInProgress)},
			{ID: k8sptr.To(int64(2)), Version: kuberikrolloutv1alpha1.VersionInfo{Tag: "v2", Revision: k8sptr.To("rev-2")}, Timestamp: metav1.Now(), BakeStatus: k8sptr.To(kuberikrolloutv1alpha1.BakeStatusFailed)},
			{ID: k8sptr.To(int64(1)), Version: kuberikrolloutv1alpha1.VersionInfo{Tag: "v1", Revision: k8sptr.To("rev-1")}, Timestamp: metav1.Now(), BakeStatus: k8sptr.To(kuberikrolloutv1alpha1.BakeStatusSucceeded)},
		}

		// reconcileGate creates a follower Environment related to "prod" whose status holds prod's
		// history, then creates its RolloutGate and updates the gate's allowed versions.
		reconcileGate := func(name string, relType kuberikv1alpha1.RelationshipType) *kuberikrolloutv1alpha1.RolloutGate {
			ctx := context.Background()

			rollout := &kuberikrolloutv1alpha1.Rollout{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: kuberikrolloutv1alpha1.RolloutSpec{
					ReleasesImagePolicy: corev1.LocalObjectReference{Name: "test-policy"},
				},
			}
			Expect(k8sClient.Create(ctx, rollout)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, rollout)).To(Succeed()) })
			for _, entry := range leaderHistory {
				rollout.Status.ReleaseCandidates = append(rollout.Status.ReleaseCandidates, *entry.Version.DeepCopy())
			}
			Expect(k8sClient.Status().Update(ctx, rollout)).To(Succeed())

			env := &kuberikv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: kuberikv1alpha1.EnvironmentSpec{
					RolloutRef:  corev1.LocalObjectReference{Name: name},
					Name:        name,
					Environment: "prod-eu-1",
					Relationship: &kuberikv1alpha1.EnvironmentRelationship{
						Environment: "prod",
						Type:        relType,
					},
					Backend: kuberikv1alpha1.BackendConfig{Type: "github", Project: "kuberik/environment-controller-testing"},
				},
			}
			Expect(k8sClient.Create(ctx, env)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, env)).To(Succeed()) })
			env.Status.EnvironmentInfos = []kuberikv1alpha1.EnvironmentInfo{
				{Environment: "prod", History: leaderHistory},
				{Environment: "prod-eu-1", Relationship: env.Spec.Relationship.DeepCopy()},
			}
			Expect(k8sClient.Status().Update(ctx, env)).To(Succeed())

			Expect(reconciler.createOrUpdateRolloutGate(ctx, env)).To(Succeed())
			Expect(env.Status.RolloutGateRef).ToNot(BeNil())
			Expect(reconciler.updateAllowedVersionsFromRelationships(ctx, env)).To(Succeed())

			gate := &kuberikrolloutv1alpha1.RolloutGate{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: env.Status.RolloutGateRef.Name, Namespace: namespace}, gate)).To(Succeed())
			return gate
		}

		It("Should allow versions the leader is deploying, baking or has succeeded with for Alongside", func() {
			gate := reconcileGate("alongside-follower", kuberikv1alpha1.RelationshipTypeAlongside)

			Expect(gate.Spec.AllowedVersions).ToNot(BeNil())
			Expect(*gate.Spec.AllowedVersions).To(ConsistOf("v5", "v3", "v1"))
			Expect(gate.Annotations["gate.kuberik.com/description"]).To(Equal(
				"This gate passes versions as soon as the prod environment starts deploying them, unless the deployment there has failed or been cancelled."))
		})

		It("Should only allow versions the leader has succeeded with for After", func() {
			gate := reconcileGate("after-follower", kuberikv1alpha1.RelationshipTypeAfter)

			Expect(gate.Spec.AllowedVersions).ToNot(BeNil())
			Expect(*gate.Spec.AllowedVersions).To(ConsistOf("v1"))
			Expect(gate.Annotations["gate.kuberik.com/description"]).To(Equal(
				"This gate is passing only for those versions that have been successfully deployed after the prod environment."))
		})
	})

	Context("relationships written by older controllers", func() {
		// fakeDeployment is a GitHub deployment as returned by the REST API
		type fakeDeployment struct {
			ID          int64           `json:"id"`
			Ref         string          `json:"ref"`
			Task        string          `json:"task"`
			Environment string          `json:"environment"`
			Payload     json.RawMessage `json:"payload"`
		}

		// newFakeGitHub serves the deployment endpoints buildRelationshipGraph reads, with every
		// deployment reporting a successful bake.
		newFakeGitHub := func(deployments []fakeDeployment) *github.Client {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				path := strings.TrimPrefix(req.URL.Path, "/repos/kuberik/environment-controller-testing/deployments")
				switch {
				case path == "":
					result := []fakeDeployment{}
					for _, d := range deployments {
						if env := req.URL.Query().Get("environment"); env != "" && env != d.Environment {
							continue
						}
						if ref := req.URL.Query().Get("ref"); ref != "" && ref != d.Ref {
							continue
						}
						result = append(result, d)
					}
					_ = json.NewEncoder(w).Encode(result)
				case strings.HasSuffix(path, "/statuses"):
					_, _ = w.Write([]byte(`[{"id":1,"state":"success","description":"Bake succeeded","environment_url":"https://eu.example.com"}]`))
				default:
					http.NotFound(w, req)
				}
			}))
			DeferCleanup(server.Close)

			client := github.NewClient(server.Client())
			baseURL, err := url.Parse(server.URL + "/")
			Expect(err).ToNot(HaveOccurred())
			client.BaseURL = baseURL
			return client
		}

		It("Should tolerate a legacy Parallel relationship in payloads and status", func() {
			ctx := context.Background()

			// Another cluster's (older) controller deploys "eu" in "Parallel" with prod
			ghClient := newFakeGitHub([]fakeDeployment{{
				ID:          7,
				Ref:         "rev-1",
				Task:        "deploy:kuberik/legacy-payload",
				Environment: "kuberik/legacy-payload/eu",
				Payload:     json.RawMessage(`{"id":"1","relationship":{"environment":"prod","type":"Parallel"},"version":{"tag":"v1","revision":"rev-1"}}`),
			}})

			env := &kuberikv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: "legacy-payload", Namespace: namespace},
				Spec: kuberikv1alpha1.EnvironmentSpec{
					RolloutRef:  corev1.LocalObjectReference{Name: "legacy-payload"},
					Name:        "legacy-payload",
					Environment: "prod",
					Backend:     kuberikv1alpha1.BackendConfig{Type: "github", Project: "kuberik/environment-controller-testing"},
				},
			}
			Expect(k8sClient.Create(ctx, env)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, env)).To(Succeed()) })

			// Status as an older controller left it: an environment the graph no longer discovers
			// still carries the removed type
			env.Status.EnvironmentInfos = []kuberikv1alpha1.EnvironmentInfo{{
				Environment:  "staging",
				Relationship: &kuberikv1alpha1.EnvironmentRelationship{Environment: "prod", Type: "Parallel"},
			}}

			graphData, err := reconciler.buildRelationshipGraph(ctx, env, ghClient)
			Expect(err).ToNot(HaveOccurred())
			Expect(graphData.environmentInfos).To(HaveKey("eu"))
			Expect(graphData.environmentInfos["eu"].Relationship.Type).To(Equal(kuberikv1alpha1.RelationshipTypeAfter))

			Expect(reconciler.updateStatus(ctx, env, graphData)).To(Succeed())

			updated := &kuberikv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: env.Name, Namespace: namespace}, updated)).To(Succeed())
			relationships := map[string]kuberikv1alpha1.EnvironmentRelationship{}
			for _, info := range updated.Status.EnvironmentInfos {
				if info.Relationship != nil {
					relationships[info.Environment] = *info.Relationship
				}
			}
			Expect(relationships).To(Equal(map[string]kuberikv1alpha1.EnvironmentRelationship{
				"eu":      {Environment: "prod", Type: kuberikv1alpha1.RelationshipTypeAfter},
				"staging": {Environment: "prod", Type: kuberikv1alpha1.RelationshipTypeAfter},
			}))
		})
	})
})
