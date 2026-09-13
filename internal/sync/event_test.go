package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/deployment"
)

type driftTestReader struct {
	pages   map[string][]deployment.Deployment
	next    map[string]string
	cursors []string
}

func (r *driftTestReader) GetByID(context.Context, string) (*deployment.Deployment, error) {
	return nil, deployment.ErrNotFound
}

func (r *driftTestReader) ListForDriftAudit(_ context.Context, cursor string, _ int) ([]deployment.Deployment, string, error) {
	r.cursors = append(r.cursors, cursor)
	return r.pages[cursor], r.next[cursor], nil
}

type driftTestApplier struct {
	applied []*platformv1alpha1.InferenceDeployment
	deleted []DeleteTarget
	failAt  int
}

func (a *driftTestApplier) Apply(_ context.Context, value *platformv1alpha1.InferenceDeployment) error {
	if a.failAt > 0 && len(a.applied)+1 == a.failAt {
		return errors.New("apply failed")
	}
	a.applied = append(a.applied, value)
	return nil
}

func (a *driftTestApplier) Delete(_ context.Context, namespace, name string) error {
	a.deleted = append(a.deleted, DeleteTarget{Namespace: namespace, Name: name})
	return nil
}

func TestToCustomResource(t *testing.T) {
	d := &deployment.Deployment{
		ID: "dep_1", TenantID: "ten_1", Name: "qwen-chat", Namespace: "tenant-acme",
		Spec: platformv1alpha1.InferenceDeploymentSpec{},
	}
	object := ToCustomResource(d)
	if object.APIVersion != "platform.inferscale.io/v1alpha1" || object.Kind != "InferenceDeployment" {
		t.Fatalf("unexpected type metadata: %#v", object.TypeMeta)
	}
	if object.Annotations["inferscale.io/deployment-id"] != d.ID {
		t.Fatal("deployment id annotation missing")
	}
	if object.Annotations["inferscale.io/revision-id"] != "" {
		t.Fatal("empty deployment must not invent a revision ID")
	}
}

func TestToCustomResourceMapsDatabaseRevisionID(t *testing.T) {
	t.Parallel()
	d := &deployment.Deployment{
		ID: "dep_1", TenantID: "ten_1", Name: "qwen-chat", Namespace: "tenant-acme",
		StableRevisionID: "stable-uuid", CandidateRevisionID: "candidate-uuid",
	}
	object := ToCustomResource(d)
	if got := object.Annotations["inferscale.io/revision-id"]; got != "candidate-uuid" {
		t.Fatalf("revision annotation=%q, want candidate DB ID", got)
	}
	d.CandidateRevisionID = ""
	object = ToCustomResource(d)
	if got := object.Annotations["inferscale.io/revision-id"]; got != "stable-uuid" {
		t.Fatalf("revision annotation=%q, want stable DB ID", got)
	}
	d.ObservedStatus.Revision.LastFailedID = "failed-uuid"
	object = ToCustomResource(d)
	if got := object.Annotations["inferscale.io/revision-id"]; got != "failed-uuid" {
		t.Fatalf("revision annotation=%q, want retained failed desired revision ID", got)
	}
	d.CandidateRevisionID = "next-candidate-uuid"
	object = ToCustomResource(d)
	if got := object.Annotations["inferscale.io/revision-id"]; got != "next-candidate-uuid" {
		t.Fatalf("revision annotation=%q, want new candidate to supersede failed tombstone", got)
	}
}

func TestRetryDelayIsCapped(t *testing.T) {
	if retryDelay(100) != 256*time.Second {
		t.Fatalf("unexpected capped delay: %v", retryDelay(100))
	}
}

func TestDriftAuditReappliesAuthoritativeStateAndPages(t *testing.T) {
	t.Parallel()
	reader := &driftTestReader{
		pages: map[string][]deployment.Deployment{
			"":   {{ID: "d1", Name: "one", Namespace: "tenant"}, {ID: "d2", Name: "two", Namespace: "tenant"}},
			"d2": {{ID: "d3", Name: "three", Namespace: "tenant"}},
		},
		next: map[string]string{"": "d2", "d2": ""},
	}
	applier := &driftTestApplier{}
	worker := NewWorker(nil, reader, applier, nil)

	if count, err := worker.RunDriftAuditOnce(context.Background()); err != nil || count != 2 {
		t.Fatalf("first drift page count=%d err=%v", count, err)
	}
	if count, err := worker.RunDriftAuditOnce(context.Background()); err != nil || count != 1 {
		t.Fatalf("second drift page count=%d err=%v", count, err)
	}
	if len(reader.cursors) != 2 || reader.cursors[0] != "" || reader.cursors[1] != "d2" {
		t.Fatalf("unexpected drift cursors: %#v", reader.cursors)
	}
	if len(applier.applied) != 3 || applier.applied[2].Name != "three" {
		t.Fatalf("unexpected repaired objects: %#v", applier.applied)
	}
}

func TestDriftAuditDoesNotAdvanceCursorAfterApplyFailure(t *testing.T) {
	t.Parallel()
	reader := &driftTestReader{
		pages: map[string][]deployment.Deployment{"": {{ID: "d1", Name: "one"}, {ID: "d2", Name: "two"}}},
		next:  map[string]string{"": "d2"},
	}
	worker := NewWorker(nil, reader, &driftTestApplier{failAt: 2}, nil)

	count, err := worker.RunDriftAuditOnce(context.Background())
	if err == nil || count != 1 {
		t.Fatalf("count=%d err=%v, want one successful apply and an error", count, err)
	}
	if worker.driftCursor != "" {
		t.Fatalf("cursor advanced past failed page: %q", worker.driftCursor)
	}
}

func TestDriftAuditEnforcesDatabaseTombstones(t *testing.T) {
	t.Parallel()
	deletedAt := time.Now()
	reader := &driftTestReader{pages: map[string][]deployment.Deployment{
		"": {{ID: "d1", Name: "deleted", Namespace: "tenant", DeletedAt: &deletedAt}},
	}}
	applier := &driftTestApplier{}
	worker := NewWorker(nil, reader, applier, nil)

	if count, err := worker.RunDriftAuditOnce(context.Background()); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	if len(applier.applied) != 0 || len(applier.deleted) != 1 || applier.deleted[0].Name != "deleted" {
		t.Fatalf("applied=%d deleted=%#v", len(applier.applied), applier.deleted)
	}
}
