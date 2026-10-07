package vm

import (
	"runtime"
	"strings"
)

const (
	MetaManaged    = "managed"
	MetaSSHUser    = "ssh_user"
	MetaSSHKey     = "ssh_key"
	MetaOSType     = "os_type"
	MetaLinkedFrom = "linked_from"

	labelPrefix = "label."
)

func LabelKey(name string) string { return labelPrefix + name }

func ApplyMeta(m *Machine) {
	m.Managed = m.ID != "" && sameID(m.Meta[MetaManaged], m.ID)
	m.Labels = nil
	for k, v := range m.Meta {
		name, ok := strings.CutPrefix(k, labelPrefix)
		if !ok || name == "" {
			continue
		}
		if m.Labels == nil {
			m.Labels = make(map[string]string)
		}
		m.Labels[name] = v
	}
}

func sameID(a, b string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
