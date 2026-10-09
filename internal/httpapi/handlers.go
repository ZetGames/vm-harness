package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/ZetGames/vm-harness/harness"
	"github.com/ZetGames/vm-harness/vm"
)

type api struct {
	m *harness.Manager
}

type handler func(w http.ResponseWriter, r *http.Request) error

func (h handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h(w, r); err != nil {
		writeError(w, err)
	}
}

type startRequest struct {
	GUI bool `json:"gui"`
}

type stopRequest struct {
	Force      bool `json:"force"`
	TimeoutSec int  `json:"timeout_sec"`
}

type snapshotRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type waitRequest struct {
	For        string `json:"for"`
	TimeoutSec int    `json:"timeout_sec"`
	harness.Access
}

func machineOp(op func(context.Context, harness.Ref) (vm.Machine, error)) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		mach, err := op(r.Context(), vmRef(r))
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, mach)
		return nil
	}
}

func copyOp(op func(context.Context, harness.Ref, harness.CopyRequest) error) handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		var req harness.CopyRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return err
		}
		if err := op(r.Context(), vmRef(r), req); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}
}

func (a *api) health(w http.ResponseWriter, _ *http.Request) error {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	return nil
}

func (a *api) providers(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, http.StatusOK, a.m.Providers(r.Context()))
	return nil
}

func (a *api) list(w http.ResponseWriter, r *http.Request) error {
	managed, err := queryBool(r, "managed")
	if err != nil {
		return err
	}
	opts := harness.ListOptions{Provider: r.URL.Query().Get("provider"), ManagedOnly: managed}
	machines, err := a.m.List(r.Context(), opts)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, machines)
	return nil
}

func (a *api) create(w http.ResponseWriter, r *http.Request) error {
	var spec vm.Spec
	if err := decodeJSON(w, r, &spec); err != nil {
		return err
	}
	if provider := r.URL.Query().Get("provider"); provider != "" {
		if spec.Provider != "" && !strings.EqualFold(spec.Provider, provider) {
			return fmt.Errorf("provider %q in the query conflicts with provider %q in the body: %w", provider, spec.Provider, vm.ErrInvalid)
		}
		spec.Provider = provider
	}
	mach, err := a.m.Create(r.Context(), spec)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, mach)
	return nil
}

func (a *api) update(w http.ResponseWriter, r *http.Request) error {
	var ch vm.Changes
	if err := decodeJSON(w, r, &ch); err != nil {
		return err
	}
	mach, err := a.m.Update(r.Context(), vmRef(r), ch)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, mach)
	return nil
}

func (a *api) delete(w http.ResponseWriter, r *http.Request) error {
	force, err := queryBool(r, "force")
	if err != nil {
		return err
	}
	if err := a.m.Delete(r.Context(), vmRef(r), force); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *api) start(w http.ResponseWriter, r *http.Request) error {
	var req startRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	mach, err := a.m.Start(r.Context(), vmRef(r), req.GUI)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, mach)
	return nil
}

func (a *api) stop(w http.ResponseWriter, r *http.Request) error {
	var req stopRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	timeout, err := seconds(req.TimeoutSec)
	if err != nil {
		return err
	}
	mach, err := a.m.Stop(r.Context(), vmRef(r), harness.StopOptions{Force: req.Force, Timeout: timeout})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, mach)
	return nil
}

func (a *api) clone(w http.ResponseWriter, r *http.Request) error {
	var opts vm.CloneOptions
	if err := decodeJSON(w, r, &opts); err != nil {
		return err
	}
	mach, err := a.m.Clone(r.Context(), vmRef(r), opts)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, mach)
	return nil
}

func (a *api) snapshots(w http.ResponseWriter, r *http.Request) error {
	snapshots, err := a.m.Snapshots(r.Context(), vmRef(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, snapshots)
	return nil
}

func (a *api) takeSnapshot(w http.ResponseWriter, r *http.Request) error {
	var req snapshotRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	snap, err := a.m.TakeSnapshot(r.Context(), vmRef(r), req.Name, req.Description)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, snap)
	return nil
}

func (a *api) restoreSnapshot(w http.ResponseWriter, r *http.Request) error {
	mach, err := a.m.RestoreSnapshot(r.Context(), vmRef(r), r.PathValue("snapshot"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, mach)
	return nil
}

func (a *api) deleteSnapshot(w http.ResponseWriter, r *http.Request) error {
	if err := a.m.DeleteSnapshot(r.Context(), vmRef(r), r.PathValue("snapshot")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *api) exec(w http.ResponseWriter, r *http.Request) error {
	var req harness.ExecRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	res, err := a.m.Exec(r.Context(), vmRef(r), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

func (a *api) writeFile(w http.ResponseWriter, r *http.Request) error {
	path, err := guestPath(r)
	if err != nil {
		return err
	}
	data, err := readUpload(w, r)
	if err != nil {
		return err
	}
	if err := a.m.WriteFile(r.Context(), vmRef(r), path, data, headerAccess(r.Header)); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (a *api) readFile(w http.ResponseWriter, r *http.Request) error {
	path, err := guestPath(r)
	if err != nil {
		return err
	}
	data, err := a.m.ReadFile(r.Context(), vmRef(r), path, headerAccess(r.Header))
	if err != nil {
		return err
	}
	writeBytes(w, "application/octet-stream", data)
	return nil
}

func (a *api) ip(w http.ResponseWriter, r *http.Request) error {
	ip, err := a.m.GuestIP(r.Context(), vmRef(r))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{"ip": ip})
	return nil
}

func (a *api) wait(w http.ResponseWriter, r *http.Request) error {
	var req waitRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	timeout, err := seconds(req.TimeoutSec)
	if err != nil {
		return err
	}
	res, err := a.m.Wait(r.Context(), vmRef(r), harness.WaitRequest{For: req.For, Timeout: timeout, Access: req.Access})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}

func (a *api) screenshot(w http.ResponseWriter, r *http.Request) error {
	png, err := a.m.Screenshot(r.Context(), vmRef(r), headerAccess(r.Header).Credentials)
	if err != nil {
		return err
	}
	writeBytes(w, "image/png", png)
	return nil
}

func (a *api) ports(w http.ResponseWriter, r *http.Request) error {
	mach, err := a.m.Get(r.Context(), vmRef(r))
	if err != nil {
		return err
	}
	forwards := mach.PortForwards
	if forwards == nil {
		forwards = []vm.PortForward{}
	}
	writeJSON(w, http.StatusOK, forwards)
	return nil
}

func (a *api) addPort(w http.ResponseWriter, r *http.Request) error {
	var pf vm.PortForward
	if err := decodeJSON(w, r, &pf); err != nil {
		return err
	}
	added, err := a.m.AddPortForward(r.Context(), vmRef(r), pf)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, added)
	return nil
}

func (a *api) removePort(w http.ResponseWriter, r *http.Request) error {
	if err := a.m.RemovePortForward(r.Context(), vmRef(r), r.PathValue("name")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
