package vm

import "context"

type Provider interface {
	Name() string
	Info(ctx context.Context) (HostInfo, error)

	List(ctx context.Context) ([]Machine, error)
	Get(ctx context.Context, ref string) (Machine, error)
	Create(ctx context.Context, spec Spec) (Machine, error)
	Delete(ctx context.Context, ref string) error
	Update(ctx context.Context, ref string, ch Changes) error
	SetMeta(ctx context.Context, ref string, meta map[string]string) error
	Clone(ctx context.Context, ref string, opts CloneOptions) (Machine, error)

	Start(ctx context.Context, ref string, gui bool) error
	Stop(ctx context.Context, ref string, force bool) error
	Pause(ctx context.Context, ref string) error
	Resume(ctx context.Context, ref string) error
	Reset(ctx context.Context, ref string) error
	Suspend(ctx context.Context, ref string) error

	Snapshots(ctx context.Context, ref string) ([]Snapshot, error)
	TakeSnapshot(ctx context.Context, ref, name, description string) error
	RestoreSnapshot(ctx context.Context, ref, name string) error
	DeleteSnapshot(ctx context.Context, ref, name string) error

	GuestIP(ctx context.Context, ref string) (string, error)
	Exec(ctx context.Context, ref string, req ExecRequest) (ExecResult, error)
	CopyTo(ctx context.Context, ref string, req CopyRequest) error
	CopyFrom(ctx context.Context, ref string, req CopyRequest) error
	Screenshot(ctx context.Context, ref string, cred Credentials) ([]byte, error)

	AddPortForward(ctx context.Context, ref string, pf PortForward) error
	RemovePortForward(ctx context.Context, ref, name string) error
}
