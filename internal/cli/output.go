package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/fl4metf/vm-harness/harness"
	"github.com/fl4metf/vm-harness/vm"
)

const (
	formatJSON = "json"
	formatText = "text"
)

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorDocument struct {
	Error errorBody `json:"error"`
}

type removal struct {
	VM          string `json:"vm"`
	Snapshot    string `json:"snapshot,omitempty"`
	PortForward string `json:"port_forward,omitempty"`
	Deleted     bool   `json:"deleted"`
}

func outputFormat(requested string, stdout io.Writer) (string, error) {
	requested = strings.ToLower(cmp.Or(requested, os.Getenv("VMH_OUTPUT")))
	if requested == formatJSON || requested == formatText {
		return requested, nil
	}
	detected := formatJSON
	if isTerminal(stdout) {
		detected = formatText
	}
	if requested != "" {
		return detected, fmt.Errorf("unknown output format %q, want json or text", requested)
	}
	return detected, nil
}

func isTerminal(stream any) bool {
	f, ok := stream.(*os.File)
	return ok && terminalFd(f.Fd())
}

func (a *app) writeJSON(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *app) emit(v any, text func(w io.Writer)) error {
	if a.format == formatJSON {
		return a.writeJSON(v)
	}
	if text == nil {
		return nil
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	text(tw)
	return tw.Flush()
}

func (a *app) emitMachine(mach vm.Machine) error {
	return a.emit(mach, func(w io.Writer) { writeMachine(w, mach) })
}

func (a *app) emitState(mach vm.Machine) error {
	return a.emit(mach, func(w io.Writer) { fmt.Fprintf(w, "%s\t%s\n", mach.Name, mach.State) })
}

func writeMachine(w io.Writer, mach vm.Machine) {
	field := func(name, value string) {
		if value != "" {
			fmt.Fprintf(w, "%s:\t%s\n", name, value)
		}
	}
	field("name", mach.Name)
	field("id", mach.ID)
	field("provider", mach.Provider)
	field("state", string(mach.State))
	field("managed", yesNo(mach.Managed))
	field("os", mach.OSType)
	field("cpus", count(mach.CPUs))
	field("memory", megabytes(mach.MemoryMB))
	field("firmware", mach.Firmware)
	field("config", mach.ConfigPath)
	field("snapshot", mach.CurrentSnapshot)
	field("console", mach.ConsoleLog)
	field("labels", formatLabels(mach.Labels))
	for i, nic := range mach.NICs {
		field("nic"+strconv.Itoa(i+1), formatNIC(nic))
	}
	for _, pf := range mach.PortForwards {
		field("forward", formatForward(pf))
	}
}

func writeMachines(w io.Writer, machines []vm.Machine) {
	fmt.Fprintln(w, "NAME\tPROVIDER\tSTATE\tMANAGED\tCPUS\tMEMORY\tOS\tLABELS")
	for _, m := range machines {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Name, m.Provider, m.State, yesNo(m.Managed), count(m.CPUs), megabytes(m.MemoryMB), m.OSType, formatLabels(m.Labels))
	}
}

func writeProviders(w io.Writer, providers []harness.ProviderStatus) {
	fmt.Fprintln(w, "NAME\tAVAILABLE\tDEFAULT\tVERSION\tDETAIL")
	for _, p := range providers {
		detail := p.Info.Binary
		if p.Error != "" {
			detail = p.Error
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Name, yesNo(p.Available), mark(p.Default), p.Info.Version, detail)
	}
	for _, p := range providers {
		for _, warning := range p.Info.Warnings {
			fmt.Fprintf(w, "warning: %s: %s\n", p.Name, warning)
		}
	}
}

func writeSnapshots(w io.Writer, snapshots []vm.Snapshot) {
	fmt.Fprintln(w, "NAME\tCURRENT\tPARENT\tDESCRIPTION")
	for _, s := range snapshots {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Name, mark(s.Current), s.Parent, s.Description)
	}
}

func writeForwards(w io.Writer, forwards []vm.PortForward) {
	fmt.Fprintln(w, "NAME\tPROTOCOL\tHOST\tGUEST")
	for _, pf := range forwards {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", pf.Name, pf.Protocol, hostPort(pf.HostIP, pf.HostPort), hostPort(pf.GuestIP, pf.GuestPort))
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func mark(b bool) string {
	if b {
		return "yes"
	}
	return ""
}

func count(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func megabytes(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n) + " MB"
}

func formatLabels(labels map[string]string) string {
	pairs := make([]string, 0, len(labels))
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		pairs = append(pairs, k+"="+labels[k])
	}
	return strings.Join(pairs, ",")
}

func formatNIC(nic vm.NIC) string {
	parts := []string{nic.Mode}
	for _, s := range []string{nic.Adapter, nic.Model, nic.MAC} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " ")
}

func formatForward(pf vm.PortForward) string {
	return fmt.Sprintf("%s %s %s -> %s", pf.Name, pf.Protocol, hostPort(pf.HostIP, pf.HostPort), hostPort(pf.GuestIP, pf.GuestPort))
}

func hostPort(host string, port int) string {
	if host == "" {
		return strconv.Itoa(port)
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}
