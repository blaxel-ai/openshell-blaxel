// Package driver implements the OpenShell (main, RFC 0012) ComputeDriver
// contract on Blaxel.
//
// Placement: the gateway, this driver and one openshell-supervisor per
// sandbox run in a trusted Blaxel "control" sandbox. Each OpenShell sandbox is
// a Blaxel workload sandbox (landlock kernel) where openshell-sandbox runs
// capability-free inside a network namespace that only has loopback, and
// serves the Sandbox Protocol on a Unix socket. The supervisor reaches that
// socket through a tunnel the driver dials into the workload sandbox. The
// workload never holds gateway or Blaxel credentials.
package driver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/blaxel-ai/openshell-blaxel/driver/gen/computev1"
	extpb "github.com/blaxel-ai/openshell-blaxel/driver/gen/extensionv1"
	"github.com/blaxel-ai/openshell-blaxel/driver/internal/blaxel"
)

const (
	DriverName    = "blaxel"
	DriverVersion = "0.2.0"

	// implementationName identifies this driver in extension negotiation.
	implementationName = "blaxel/openshell-driver-blaxel"
	computeContract    = "openshell.compute.contract"

	// DefaultAdmissionPolicy is the gateway's default DriverAdmissionConfig
	// acknowledgement (crates/openshell-core/src/resource_admission.rs). The
	// gateway re-reads it before every Validate/Create/Start and requires
	// byte equality, so it must be stable.
	DefaultAdmissionPolicy = `v1:{"allow_driver_config":false,"resource_admission":{"enabled":true,"required_labels":{"openshell.ai/sandbox-attachable":"true","openshell.ai/sandbox-attachable-workspace":"${workspace}"}}}`

	labelManaged   = "openshell.ai/managed-by"
	labelID        = "openshell.ai/sandbox-id"
	labelName      = "openshell.ai/sandbox-name"
	labelWorkspace = "openshell.ai/sandbox-workspace"
	labelNamespace = "openshell.ai/sandbox-namespace"
	// labelOwner scopes a sandbox to one driver instance (one gateway), so
	// several gateways can share a Blaxel workspace without adopting, or
	// cleaning up, each other's sandboxes.
	labelOwner = "openshell.ai/driver-owner"

	// DefaultOwner owns sandboxes created before labelOwner existed.
	DefaultOwner = "blaxel"
)

type Config struct {
	// Owner identifies this driver instance; see labelOwner.
	Owner        string
	Region       string
	DefaultImage string
	MemoryMiB    int
	// ExtraArgs is spec.runtime.extraArgs, e.g. {"landlock": "enabled"}.
	// OpenShell main requires Landlock ABI >= 3 in the workload kernel.
	ExtraArgs     map[string]string
	Packages      []string
	InstallClaude bool
	TunnelPort    int

	// Workload-side artifacts uploaded into each workload sandbox.
	SandboxBinary string // linux x86_64 openshell-sandbox (main)
	TunnelBinary  string // linux x86_64 os-tunnel

	// Control-side supervisor.
	SupervisorBinary string // openshell-supervisor (main)
	StateDir         string // per-sandbox descriptor, auth bundle, logs
	GatewayEndpoint  string // e.g. https://127.0.0.1:17670
	GatewayCA        string // gateway TLS: CA, client cert and key for the supervisor
	GatewayCert      string
	GatewayKey       string
	LogLevel         string
}

type Driver struct {
	pb.UnimplementedComputeDriverServer
	cfg Config
	bl  *blaxel.Client
	log *slog.Logger

	mu   sync.Mutex
	recs map[string]*record // by OpenShell sandbox id
	subs map[chan *pb.WatchSandboxesEvent]struct{}
}

type record struct {
	id, name, namespace, workspace string
	blxName, url, image            string

	conds    []*pb.DriverCondition
	deleting bool
	stopped  bool

	// Current generation's runtime (nil when stopped).
	run *runtime
}

func New(cfg Config, bl *blaxel.Client, log *slog.Logger) *Driver {
	return &Driver{cfg: cfg, bl: bl, log: log, recs: map[string]*record{}, subs: map[chan *pb.WatchSandboxesEvent]struct{}{}}
}

// ---- snapshots and events ----

func (d *Driver) snapshotLocked(r *record) *pb.DriverSandbox {
	conds := make([]*pb.DriverCondition, len(r.conds))
	copy(conds, r.conds)
	return &pb.DriverSandbox{
		Id: r.id, Name: r.name, Namespace: r.namespace, Workspace: r.workspace,
		Status: &pb.DriverSandboxStatus{Name: r.blxName, InstanceId: r.blxName, Conditions: conds, Deleting: r.deleting},
	}
}

func (d *Driver) broadcastLocked(ev *pb.WatchSandboxesEvent) {
	for ch := range d.subs {
		select {
		case ch <- ev:
		default: // slow watcher; the gateway re-lists on reconnect
		}
	}
}

func (d *Driver) setStatus(r *record, conds ...*pb.DriverCondition) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r.conds = conds
	d.broadcastLocked(&pb.WatchSandboxesEvent{Payload: &pb.WatchSandboxesEvent_Sandbox{
		Sandbox: &pb.WatchSandboxesSandboxEvent{Sandbox: d.snapshotLocked(r)}}})
}

func cond(typ, statusVal, reason, msg string) *pb.DriverCondition {
	return &pb.DriverCondition{Type: typ, Status: statusVal, Reason: reason, Message: msg, TransitionTime: timestamppb.Now()}
}

// Conditions the gateway maps to phases (crates/openshell-server/src/compute/mod.rs
// derive_phase). The driver never reports Ready=True: readiness is the
// supervisor session (driver_reports_runtime_readiness=false).
func starting(msg string) *pb.DriverCondition { return cond("Ready", "False", "Starting", msg) }
func failed(reason, msg string) *pb.DriverCondition {
	return cond("Ready", "False", reason, msg) // non-transient reason => Error
}
func stoppedConds(msg string) []*pb.DriverCondition {
	return []*pb.DriverCondition{cond("Ready", "False", "ContainerStopped", msg), cond("Suspended", "True", "ComputeStopped", msg)}
}

func (d *Driver) event(r *record, typ, reason, msg string) {
	d.log.Info("platform event", "sandbox", r.name, "reason", reason, "msg", msg)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.broadcastLocked(&pb.WatchSandboxesEvent{Payload: &pb.WatchSandboxesEvent_PlatformEvent{
		PlatformEvent: &pb.WatchSandboxesPlatformEvent{SandboxId: r.id, Event: &pb.DriverPlatformEvent{
			EventTime: timestamppb.Now(), Source: "blaxel", Type: typ, Reason: reason, Message: msg,
			Metadata: map[string]string{"blaxel_sandbox": r.blxName},
		}},
	}})
}

// ---- capabilities ----

func peerMetadata() *extpb.PeerMetadata {
	return &extpb.PeerMetadata{
		ProtocolVersion:       &extpb.ProtocolVersion{Major: 1, Minor: 0},
		ImplementationName:    implementationName,
		ImplementationVersion: DriverVersion,
		SupportedCapabilities: []string{computeContract},
		RequiredCapabilities:  []string{computeContract},
	}
}

// validateGateway mirrors validate_gateway_metadata: protocol major 1 and the
// base compute contract.
func validateGateway(g *extpb.PeerMetadata) error {
	if g == nil || g.GetProtocolVersion() == nil {
		return errors.New("gateway sent no extension metadata; upgrade the gateway")
	}
	if g.GetProtocolVersion().GetMajor() != 1 {
		return fmt.Errorf("unsupported compute protocol %d.%d (driver speaks 1.x)", g.GetProtocolVersion().GetMajor(), g.GetProtocolVersion().GetMinor())
	}
	for _, list := range [][]string{g.GetSupportedCapabilities()} {
		for _, c := range list {
			if c == computeContract {
				return nil
			}
		}
	}
	return fmt.Errorf("gateway does not support %s", computeContract)
}

func (d *Driver) GetCapabilities(_ context.Context, req *pb.GetCapabilitiesRequest) (*pb.GetCapabilitiesResponse, error) {
	if err := validateGateway(req.GetGateway()); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &pb.GetCapabilitiesResponse{
		DriverName: DriverName, DriverVersion: DriverVersion, DefaultImage: d.cfg.DefaultImage,
		// The gateway stops sandboxes on shutdown and restarts them with
		// fresh launch credentials: a sandbox pins its first supervisor, so
		// generations cannot survive a control-plane restart anyway.
		GatewayManagesLifecycle:       true,
		SupportsSandboxAuthentication: false,
		DriverReportsRuntimeReadiness: false,
		ResourceCapabilities: &pb.ResourceCapabilities{
			Cpu: &pb.CpuResourceCapabilities{LimitSupported: false},
			Gpu: &pb.GpuResourceCapabilities{},
		},
		Extension:               peerMetadata(),
		ResourceAdmissionPolicy: DefaultAdmissionPolicy,
	}, nil
}

func (d *Driver) EnsureWorkspace(context.Context, *pb.EnsureWorkspaceRequest) (*pb.EnsureWorkspaceResponse, error) {
	return &pb.EnsureWorkspaceResponse{}, nil
}

func (d *Driver) DeleteWorkspace(context.Context, *pb.DeleteWorkspaceRequest) (*pb.DeleteWorkspaceResponse, error) {
	return &pb.DeleteWorkspaceResponse{}, nil
}

func (d *Driver) ValidateSandboxCreate(_ context.Context, req *pb.ValidateSandboxCreateRequest) (*pb.ValidateSandboxCreateResponse, error) {
	sb := req.GetSandbox()
	if sb.GetId() == "" || sb.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox id and name are required")
	}
	if sb.GetSpec().GetResourceRequirements().GetGpu() != nil {
		return nil, status.Error(codes.FailedPrecondition, "the Blaxel driver does not support GPU sandboxes")
	}
	if _, err := d.memoryMiB(sb); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if wi := sb.GetSpec().GetWorkloadIdentity(); wi != nil {
		for _, sel := range []string{wi.GetUser(), wi.GetGroup()} {
			if sel != "" && sel != workloadUser && sel != fmt.Sprint(workloadUID) {
				return nil, status.Errorf(codes.InvalidArgument, "workload identity %q is not supported; sandboxes run as %s (%d)", sel, workloadUser, workloadUID)
			}
		}
	}
	return &pb.ValidateSandboxCreateResponse{}, nil
}

func (d *Driver) GetSandbox(_ context.Context, req *pb.GetSandboxRequest) (*pb.GetSandboxResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := d.find(req.GetSandboxId(), req.GetName())
	if r == nil {
		return nil, status.Errorf(codes.NotFound, "sandbox %q not found", req.GetSandboxId())
	}
	return &pb.GetSandboxResponse{Sandbox: d.snapshotLocked(r)}, nil
}

func (d *Driver) ListSandboxes(context.Context, *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*pb.DriverSandbox, 0, len(d.recs))
	for _, r := range d.recs {
		out = append(out, d.snapshotLocked(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	return &pb.ListSandboxesResponse{Sandboxes: out}, nil
}

func (d *Driver) WatchSandboxes(_ *pb.WatchSandboxesRequest, stream pb.ComputeDriver_WatchSandboxesServer) error {
	ch := make(chan *pb.WatchSandboxesEvent, 256)
	d.mu.Lock()
	for _, r := range d.recs {
		ch <- &pb.WatchSandboxesEvent{Payload: &pb.WatchSandboxesEvent_Sandbox{
			Sandbox: &pb.WatchSandboxesSandboxEvent{Sandbox: d.snapshotLocked(r)}}}
	}
	d.subs[ch] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.subs, ch)
		d.mu.Unlock()
	}()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case ev := <-ch:
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

func (d *Driver) find(id, name string) *record {
	if r, ok := d.recs[id]; ok {
		return r
	}
	if name != "" {
		for _, r := range d.recs {
			if r.name == name || r.blxName == name {
				return r
			}
		}
	}
	return nil
}

// ---- lifecycle ----

func (d *Driver) CreateSandbox(_ context.Context, req *pb.CreateSandboxRequest) (*pb.CreateSandboxResponse, error) {
	sb := req.GetSandbox()
	d.mu.Lock()
	if d.recs[sb.GetId()] != nil {
		d.mu.Unlock()
		return &pb.CreateSandboxResponse{}, nil // idempotent retry
	}
	r := &record{id: sb.GetId(), name: sb.GetName(), namespace: sb.GetNamespace(), workspace: sb.GetWorkspace(),
		blxName: blaxelName(sb.GetName(), sb.GetId())}
	d.recs[r.id] = r
	d.mu.Unlock()

	d.setStatus(r, starting("creating Blaxel sandbox "+r.blxName))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := d.provision(ctx, r, sb); err != nil {
			d.log.Error("provision failed", "sandbox", r.name, "err", err)
			d.setStatus(r, failed("ProvisionFailed", err.Error()))
		}
	}()
	return &pb.CreateSandboxResponse{}, nil
}

func (d *Driver) StopSandbox(ctx context.Context, req *pb.StopSandboxRequest) (*pb.StopSandboxResponse, error) {
	d.mu.Lock()
	r := d.find(req.GetSandboxId(), req.GetName())
	if r == nil {
		d.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "sandbox %q not found", req.GetSandboxId())
	}
	r.stopped = true
	d.mu.Unlock()
	d.teardown(ctx, r)
	d.setStatus(r, stoppedConds("stopped")...)
	return &pb.StopSandboxResponse{}, nil
}

func (d *Driver) StartSandbox(_ context.Context, req *pb.StartSandboxRequest) (*pb.StartSandboxResponse, error) {
	d.mu.Lock()
	r := d.find(req.GetSandboxId(), req.GetName())
	if r == nil {
		d.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "sandbox %q not found", req.GetSandboxId())
	}
	running := r.run != nil && !r.stopped
	d.mu.Unlock()
	if g := req.GetGenerationId(); g != "" && !validGeneration(g) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid generation_id %q", g)
	}
	if running && len(req.GetLaunchAuthentication()) == 0 {
		return &pb.StartSandboxResponse{}, nil
	}
	if len(req.GetLaunchAuthentication()) == 0 {
		return nil, status.Error(codes.FailedPrecondition, "start requires launch_authentication")
	}
	auth := req.GetLaunchAuthentication()
	d.mu.Lock()
	r.stopped = false
	d.mu.Unlock()
	d.setStatus(r, starting("starting a new generation"))
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		d.teardown(ctx, r)
		if err := d.launch(ctx, r, auth, nil); err != nil {
			d.setStatus(r, failed("StartFailed", err.Error()))
		}
	}()
	return &pb.StartSandboxResponse{}, nil
}

func (d *Driver) DeleteSandbox(ctx context.Context, req *pb.DeleteSandboxRequest) (*pb.DeleteSandboxResponse, error) {
	d.mu.Lock()
	r := d.find(req.GetSandboxId(), req.GetName())
	if r == nil {
		d.mu.Unlock()
		return &pb.DeleteSandboxResponse{Deleted: false}, nil
	}
	r.deleting, r.stopped = true, true
	d.mu.Unlock()
	d.setStatus(r, cond("Ready", "False", "Deleting", "deleting Blaxel sandbox"))
	d.teardown(ctx, r)
	err := d.bl.DeleteSandbox(ctx, r.blxName)
	if err != nil && !errors.Is(err, blaxel.ErrNotFound) {
		return nil, status.Errorf(codes.Unavailable, "delete %s: %v", r.blxName, err)
	}
	d.mu.Lock()
	delete(d.recs, r.id)
	d.broadcastLocked(&pb.WatchSandboxesEvent{Payload: &pb.WatchSandboxesEvent_Deleted{
		Deleted: &pb.WatchSandboxesDeletedEvent{SandboxId: r.id}}})
	d.mu.Unlock()
	d.removeState(r)
	return &pb.DeleteSandboxResponse{Deleted: err == nil}, nil
}

// ---- recovery ----

// Recover rebuilds records from Blaxel labels after a driver restart. The
// previous supervisors died with the old driver (parent-liveness pipe), and a
// sandbox pins its first supervisor, so recovered sandboxes are reported
// stopped; the gateway (or `openshell sandbox start`) relaunches them with
// fresh launch credentials.
func (d *Driver) Recover(ctx context.Context) error {
	all, err := d.bl.ListSandboxes(ctx)
	if err != nil {
		return err
	}
	for _, s := range all {
		l := s.Labels
		if l[labelManaged] != "openshell-driver-blaxel" || l[labelID] == "" || !d.owns(l) {
			continue
		}
		r := &record{id: l[labelID], name: l[labelName], workspace: l[labelWorkspace], namespace: l[labelNamespace],
			blxName: s.Name, url: s.URL, stopped: true}
		d.mu.Lock()
		d.recs[r.id] = r
		d.mu.Unlock()
		// Stop whatever the previous generation left running in the VM.
		d.stopWorkload(ctx, r)
		d.setStatus(r, stoppedConds("driver restarted; start to relaunch with fresh credentials")...)
		d.log.Info("recovered sandbox as stopped", "sandbox", r.name)
	}
	return nil
}

// Close stops every supervisor and tunnel (workload VMs keep running and
// are stopped on the next start or delete).
func (d *Driver) Close() {
	d.mu.Lock()
	recs := make([]*record, 0, len(d.recs))
	for _, r := range d.recs {
		recs = append(recs, r)
	}
	d.mu.Unlock()
	for _, r := range recs {
		d.stopRuntime(r)
	}
}
