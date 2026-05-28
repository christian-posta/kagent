package handlers

import (
	"net/http"
	"sort"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	api "github.com/kagent-dev/kagent/go/api/httpapi"
	"github.com/kagent-dev/kagent/go/core/internal/httpserver/errors"
	"github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend/substrate/harness"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// SubstrateHandler serves read-only observability data sourced from the
// substrate ate-api Control RPCs (ListWorkers, ListActors). It is only
// registered when the controller was started with the substrate harness
// backend enabled.
type SubstrateHandler struct {
	Client *harness.Client
}

func NewSubstrateHandler(c *harness.Client) *SubstrateHandler {
	return &SubstrateHandler{Client: c}
}

type workerDTO struct {
	WorkerNamespace string `json:"workerNamespace"`
	WorkerPool      string `json:"workerPool"`
	WorkerPod       string `json:"workerPod"`
	ActorNamespace  string `json:"actorNamespace,omitempty"`
	ActorTemplate   string `json:"actorTemplate,omitempty"`
	ActorID         string `json:"actorId,omitempty"`
	IP              string `json:"ip,omitempty"`
	Version         int64  `json:"version"`
}

type actorDTO struct {
	ActorID                string `json:"actorId"`
	Version                int64  `json:"version"`
	ActorTemplateNamespace string `json:"actorTemplateNamespace,omitempty"`
	ActorTemplateName      string `json:"actorTemplateName,omitempty"`
	Status                 string `json:"status"`
	AteomPodNamespace      string `json:"ateomPodNamespace,omitempty"`
	AteomPodName           string `json:"ateomPodName,omitempty"`
	AteomPodIP             string `json:"ateomPodIp,omitempty"`
	LastSnapshot           string `json:"lastSnapshot,omitempty"`
	InProgressSnapshot     string `json:"inProgressSnapshot,omitempty"`
}

func (h *SubstrateHandler) HandleListWorkers(w ErrorResponseWriter, r *http.Request) {
	log := ctrllog.FromContext(r.Context()).WithName("substrate-handler").WithValues("operation", "list-workers")
	if h.Client == nil {
		w.RespondWithError(errors.NewNotImplementedError("substrate backend is not enabled on this controller", nil))
		return
	}
	workers, err := h.Client.ListWorkers(r.Context())
	if err != nil {
		log.Error(err, "ListWorkers failed")
		w.RespondWithError(errors.NewInternalServerError("failed to list substrate workers", err))
		return
	}
	// Stable order across refreshes — ListWorkers has no defined order, so
	// without this the UI rows shuffle every poll.
	sort.SliceStable(workers, func(i, j int) bool {
		if workers[i].GetWorkerNamespace() != workers[j].GetWorkerNamespace() {
			return workers[i].GetWorkerNamespace() < workers[j].GetWorkerNamespace()
		}
		if workers[i].GetWorkerPool() != workers[j].GetWorkerPool() {
			return workers[i].GetWorkerPool() < workers[j].GetWorkerPool()
		}
		return workers[i].GetWorkerPod() < workers[j].GetWorkerPod()
	})
	out := make([]workerDTO, 0, len(workers))
	for _, wkr := range workers {
		out = append(out, workerDTO{
			WorkerNamespace: wkr.GetWorkerNamespace(),
			WorkerPool:      wkr.GetWorkerPool(),
			WorkerPod:       wkr.GetWorkerPod(),
			ActorNamespace:  wkr.GetActorNamespace(),
			ActorTemplate:   wkr.GetActorTemplate(),
			ActorID:         wkr.GetActorId(),
			IP:              wkr.GetIp(),
			Version:         wkr.GetVersion(),
		})
	}
	RespondWithJSON(w, http.StatusOK, api.NewResponse(out, "Successfully listed substrate workers", false))
}

func (h *SubstrateHandler) HandleListActors(w ErrorResponseWriter, r *http.Request) {
	log := ctrllog.FromContext(r.Context()).WithName("substrate-handler").WithValues("operation", "list-actors")
	if h.Client == nil {
		w.RespondWithError(errors.NewNotImplementedError("substrate backend is not enabled on this controller", nil))
		return
	}
	actors, err := h.Client.ListActors(r.Context())
	if err != nil {
		log.Error(err, "ListActors failed")
		w.RespondWithError(errors.NewInternalServerError("failed to list substrate actors", err))
		return
	}
	// Stable order: running actors first, then by template, then by id.
	sort.SliceStable(actors, func(i, j int) bool {
		ri := actors[i].GetStatus() == ateapipb.Actor_STATUS_RUNNING
		rj := actors[j].GetStatus() == ateapipb.Actor_STATUS_RUNNING
		if ri != rj {
			return ri
		}
		if actors[i].GetActorTemplateNamespace() != actors[j].GetActorTemplateNamespace() {
			return actors[i].GetActorTemplateNamespace() < actors[j].GetActorTemplateNamespace()
		}
		if actors[i].GetActorTemplateName() != actors[j].GetActorTemplateName() {
			return actors[i].GetActorTemplateName() < actors[j].GetActorTemplateName()
		}
		return actors[i].GetActorId() < actors[j].GetActorId()
	})
	out := make([]actorDTO, 0, len(actors))
	for _, a := range actors {
		out = append(out, actorDTO{
			ActorID:                a.GetActorId(),
			Version:                a.GetVersion(),
			ActorTemplateNamespace: a.GetActorTemplateNamespace(),
			ActorTemplateName:      a.GetActorTemplateName(),
			Status:                 actorStatusString(a.GetStatus()),
			AteomPodNamespace:      a.GetAteomPodNamespace(),
			AteomPodName:           a.GetAteomPodName(),
			AteomPodIP:             a.GetAteomPodIp(),
			LastSnapshot:           a.GetLastSnapshot(),
			InProgressSnapshot:     a.GetInProgressSnapshot(),
		})
	}
	RespondWithJSON(w, http.StatusOK, api.NewResponse(out, "Successfully listed substrate actors", false))
}

func actorStatusString(s ateapipb.Actor_Status) string {
	switch s {
	case ateapipb.Actor_STATUS_RUNNING:
		return "Running"
	case ateapipb.Actor_STATUS_SUSPENDED:
		return "Suspended"
	case ateapipb.Actor_STATUS_RESUMING:
		return "Resuming"
	case ateapipb.Actor_STATUS_SUSPENDING:
		return "Suspending"
	default:
		return "Unspecified"
	}
}
