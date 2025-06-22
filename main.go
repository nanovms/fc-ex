// This was taken from Julia Evans' gist on running firecracker
// workloads with go:
//
// https://gist.github.com/jvns/bb0a93e3b84a5e8344c6b24b57b2b490

package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	models "github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

var kpath = "/home/ubuntu/.ops/nightly/kernel.img"
var fpath = "/home/ubuntu/firecracker/firecracker"

type CreateRequest struct {
	RootDrivePath string `json:"root_image_path"`
}

type CreateResponse struct {
	IpAddress string `json:"ip_address"`
	ID        string `json:"id"`
}

type DeleteRequest struct {
	ID string `json:"id"`
}

var runningVMs map[string]RunningFirecracker = make(map[string]RunningFirecracker)
var ipByte byte = 3

func main() {
	http.HandleFunc("/create", createRequestHandler)
	http.HandleFunc("/delete", deleteRequestHandler)
	defer cleanup()

	log.Fatal(http.ListenAndServe(":8080", nil))
}

func cleanup() {
	for _, running := range runningVMs {
		running.machine.StopVMM()
	}
}

func deleteRequestHandler(w http.ResponseWriter, r *http.Request) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Fatalf("failed to read body, %s", err)
	}
	var req DeleteRequest
	json.Unmarshal([]byte(body), &req)
	if err != nil {
		log.Fatalf(err.Error())
	}

	running := runningVMs[req.ID]
	running.machine.StopVMM()
	delete(runningVMs, req.ID)
}

func createRequestHandler(w http.ResponseWriter, r *http.Request) {
	ipByte += 1
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Fatalf("failed to read body, %s", err)
	}
	var req CreateRequest
	json.Unmarshal([]byte(body), &req)
	opts := getOptions(ipByte, req)
	running, err := opts.createVMM(context.Background())
	if err != nil {
		log.Fatalf(err.Error())
	}

	id := pseudo_uuid()
	resp := CreateResponse{
		IpAddress: opts.FcIP,
		ID:        id,
	}
	response, err := json.Marshal(&resp)
	if err != nil {
		log.Fatalf("failed to marshal json, %s", err)
	}
	w.Header().Add("Content-Type", "application/json")
	w.Write(response)

	runningVMs[id] = *running

	go func() {
		defer running.cancelCtx()
		running.machine.Wait(running.ctx)
	}()
}

func pseudo_uuid() string {

	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		log.Fatalf("failed to generate uuid, %s", err)
	}

	return fmt.Sprintf("%X-%X-%X-%X-%X", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func getOptions(id byte, req CreateRequest) options {
	fc_ip := net.IPv4(172, 17, 0, id).String()
	gateway_ip := "172.17.0.1"
	docker_mask_long := "255.255.255.0"
	bootArgs := "ro console=ttyS0 noapic reboot=k panic=1 pci=off nomodules random.trust_cpu=on "
	bootArgs = bootArgs + fmt.Sprintf("ip=%s::%s:%s::eth0:off", fc_ip, gateway_ip, docker_mask_long)
	return options{
		FcBinary:        fpath,
		FcKernelImage:   kpath,
		FcKernelCmdLine: bootArgs,
		FcRootDrivePath: req.RootDrivePath,
		FcSocketPath:    fmt.Sprintf("/tmp/firecracker-%d.sock", id),
		TapMacAddr:      fmt.Sprintf("02:FC:00:00:00:%02x", id),
		TapDev:          fmt.Sprintf("fc-tap-%d", id),
		FcIP:            fc_ip,
		FcCPUCount:      1,
		FcMemSz:         512,
	}
}

type RunningFirecracker struct {
	ctx       context.Context
	cancelCtx context.CancelFunc
	machine   *firecracker.Machine
}

func (opts *options) createVMM(ctx context.Context) (*RunningFirecracker, error) {
	vmmCtx, vmmCancel := context.WithCancel(ctx)
	fcCfg := opts.getConfig()

	cmd := firecracker.VMCommandBuilder{}.
		WithBin(opts.FcBinary).
		WithSocketPath(fcCfg.SocketPath).
		WithStdin(os.Stdin).
		WithStdout(os.Stdout).
		WithStderr(os.Stderr).
		Build(ctx)

	machineOpts := []firecracker.Opt{
		firecracker.WithProcessRunner(cmd),
	}

	exec.Command("ip", "link", "del", opts.TapDev).Run()
	if err := exec.Command("ip", "tuntap", "add", "dev", opts.TapDev, "mode", "tap").Run(); err != nil {
		return nil, fmt.Errorf("Failed creating ip link: %s", err)
	}

	if err := exec.Command("rm", "-f", opts.FcSocketPath).Run(); err != nil {
		return nil, fmt.Errorf("Failed to delete old socket path: %s", err)
	}

	if err := exec.Command("ip", "link", "set", opts.TapDev, "up").Run(); err != nil {
		return nil, fmt.Errorf("Failed creating ip link: %s", err)
	}

	if err := exec.Command("sysctl", "-w", fmt.Sprintf("net.ipv4.conf.%s.proxy_arp=1", opts.TapDev)).Run(); err != nil {
		return nil, fmt.Errorf("Failed doing first sysctl: %s", err)
	}

	if err := exec.Command("sysctl", "-w", fmt.Sprintf("net.ipv6.conf.%s.disable_ipv6=1", opts.TapDev)).Run(); err != nil {
		return nil, fmt.Errorf("Failed doing second sysctl: %s", err)
	}

	m, err := firecracker.NewMachine(vmmCtx, fcCfg, machineOpts...)
	if err != nil {
		return nil, fmt.Errorf("Failed creating machine: %s", err)
	}

	if err := m.Start(vmmCtx); err != nil {
		return nil, fmt.Errorf("Failed to start machine: %v", err)
	}

	installSignalHandlers(vmmCtx, m)
	return &RunningFirecracker{
		ctx:       vmmCtx,
		cancelCtx: vmmCancel,
		machine:   m,
	}, nil
}

type options struct {
	Id string `long:"id" description:"Jailer VMM id"`
	// maybe make this an int instead
	IpId            byte   `byte:"id" description:"an ip we use to generate an ip address"`
	FcBinary        string `long:"firecracker-binary" description:"Path to firecracker binary"`
	FcKernelImage   string `long:"kernel" description:"Path to the kernel image"`
	FcKernelCmdLine string `long:"kernel-opts" description:"Kernel commandline"`
	FcRootDrivePath string `long:"root-drive" description:"Path to root disk image"`
	FcSocketPath    string `long:"socket-path" short:"s" description:"path to use for firecracker socket"`
	TapMacAddr      string `long:"tap-mac-addr" description:"tap macaddress"`
	TapDev          string `long:"tap-dev" description:"tap device"`
	FcCPUCount      int64  `long:"ncpus" short:"c" description:"Number of CPUs"`
	FcMemSz         int64  `long:"memory" short:"m" description:"VM memory, in MiB"`
	FcIP            string `long:"fc-ip" description:"IP address of the VM"`
}

func (opts *options) getConfig() firecracker.Config {
	return firecracker.Config{
		VMID:            opts.Id,
		SocketPath:      opts.FcSocketPath,
		KernelImagePath: opts.FcKernelImage,
		KernelArgs:      opts.FcKernelCmdLine,
		Drives: []models.Drive{
			models.Drive{
				DriveID:      firecracker.String("1"),
				PathOnHost:   &opts.FcRootDrivePath,
				IsRootDevice: firecracker.Bool(true),
				IsReadOnly:   firecracker.Bool(false),
			},
		},
		NetworkInterfaces: []firecracker.NetworkInterface{
			firecracker.NetworkInterface{
				StaticConfiguration: &firecracker.StaticNetworkConfiguration{
					MacAddress:  opts.TapMacAddr,
					HostDevName: opts.TapDev,
				},
			},
		},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(opts.FcCPUCount),
			MemSizeMib: firecracker.Int64(opts.FcMemSz),
		},
	}
}

func installSignalHandlers(ctx context.Context, m *firecracker.Machine) {
	go func() {
		signal.Reset(os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)

		for {
			switch s := <-c; {
			case s == syscall.SIGTERM || s == os.Interrupt:
				log.Printf("Caught SIGINT, requesting clean shutdown")
				m.Shutdown(ctx)
			case s == syscall.SIGQUIT:
				log.Printf("Caught SIGTERM, forcing shutdown")
				m.StopVMM()
			}
		}
	}()
}
