package virtualbox

import (
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/fl4metf/vm-harness/vm"
)

func lines(s string) []string {
	s = strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

var keyLine = regexp.MustCompile(`^("[^"]*"|[A-Za-z][^="\s]*)=`)

type pair struct {
	key, value string
}

func parsePairs(out string) []pair {
	var pairs []pair
	text := strings.ReplaceAll(out, "\r\n", "\n")
	for text != "" {
		line, _, _ := strings.Cut(text, "\n")
		if m := keyLine.FindStringSubmatch(line); m != nil {
			value, n := unquote(text[len(m[0]):])
			pairs = append(pairs, pair{strings.Trim(m[1], `"`), value})
			text = text[len(m[0])+n:]
		}
		_, text, _ = strings.Cut(text, "\n")
	}
	return pairs
}

func unquote(s string) (string, int) {
	if !strings.HasPrefix(s, `"`) {
		end := strings.IndexByte(s, '\n')
		if end < 0 {
			end = len(s)
		}
		return s[:end], end
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\'):
			i++
			b.WriteByte(s[i])
		case c == '"':
			return b.String(), i + 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), len(s)
}

type vmInfo struct {
	fields   map[string]string
	forwards []vm.PortForward
	natRules map[int][]string
}

func (i vmInfo) id() string   { return i.fields["UUID"] }
func (i vmInfo) name() string { return i.fields["name"] }
func (i vmInfo) dir() string  { return filepath.Dir(i.fields["CfgFile"]) }

func (i vmInfo) state() vm.State { return stateOf(i.fields["VMState"]) }

func (i vmInfo) numbered(prefix string) []string {
	var values []string
	for n := 1; ; n++ {
		v, ok := i.fields[prefix+strconv.Itoa(n)]
		if !ok {
			return values
		}
		values = append(values, v)
	}
}

func (i vmInfo) dvdImages() []string {
	var images []string
	for _, slot := range dvdSlots(i.fields) {
		if image := i.fields[slot]; filepath.IsAbs(image) {
			images = append(images, image)
		}
	}
	return images
}

func (i vmInfo) ownImages(iso string) []string {
	var own []string
	for _, image := range i.dvdImages() {
		if samePath(filepath.Dir(image), i.dir()) || iso != "" && samePath(image, iso) {
			own = append(own, image)
		}
	}
	return own
}

var slotKey = regexp.MustCompile(`^(.+)-\d+-\d+$`)

func (i vmInfo) mediaSlots() []string {
	controllers := make(map[string]bool)
	for key, value := range i.fields {
		if strings.HasPrefix(key, "storagecontrollername") {
			controllers[value] = true
		}
	}
	var slots []string
	for key, value := range i.fields {
		if m := slotKey.FindStringSubmatch(key); m != nil && controllers[m[1]] && value != "none" && value != "emptydrive" {
			slots = append(slots, key)
		}
	}
	slices.Sort(slots)
	return slots
}

func dvdSlots(f map[string]string) []string {
	var slots []string
	for key := range f {
		if ctl, slot, ok := strings.Cut(key, "-IsEjected-"); ok {
			slots = append(slots, ctl+"-"+slot)
		}
	}
	slices.Sort(slots)
	return slots
}

func parseVMInfo(out string) vmInfo {
	info := vmInfo{fields: make(map[string]string), natRules: make(map[int][]string)}
	nic := 0
	for _, kv := range parsePairs(out) {
		if strings.HasPrefix(kv.key, "Forwarding(") {
			info.natRules[nic] = append(info.natRules[nic], ruleName(kv.value))
			if pf, ok := parseForward(kv.value); ok && nic == 1 {
				info.forwards = append(info.forwards, pf)
			}
			continue
		}
		if _, seen := info.fields[kv.key]; seen {
			continue
		}
		if n, ok := nicIndex(kv.key); ok {
			nic = n
		}
		info.fields[kv.key] = kv.value
	}
	return info
}

func nicIndex(key string) (int, bool) {
	rest, ok := strings.CutPrefix(key, "nic")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil
}

func ruleName(rule string) string {
	f := strings.Split(rule, ",")
	return strings.Join(f[:max(len(f)-5, 1)], ",")
}

func parseForward(rule string) (vm.PortForward, bool) {
	f := strings.Split(rule, ",")
	if len(f) != 6 {
		return vm.PortForward{}, false
	}
	hostPort, err1 := strconv.Atoi(f[3])
	guestPort, err2 := strconv.Atoi(f[5])
	if err1 != nil || err2 != nil {
		return vm.PortForward{}, false
	}
	return vm.PortForward{Name: f[0], Protocol: f[1], HostIP: f[2], HostPort: hostPort, GuestIP: f[4], GuestPort: guestPort}, true
}

type vmEntry struct {
	name, id string
}

func parseVMList(out string) []vmEntry {
	var entries []vmEntry
	for _, line := range lines(out) {
		i := strings.LastIndex(line, `" {`)
		if i < 1 || !strings.HasPrefix(line, `"`) || !strings.HasSuffix(line, "}") {
			continue
		}
		entries = append(entries, vmEntry{name: line[1:i], id: line[i+3 : len(line)-1]})
	}
	return entries
}

const metaPrefix = "vmh/"

func parseExtradata(out string) map[string]string {
	data := make(map[string]string)
	for _, line := range lines(out) {
		rest, ok := strings.CutPrefix(line, "Key: ")
		if !ok {
			continue
		}
		if key, value, ok := strings.Cut(rest, ", Value: "); ok {
			data[key] = value
		}
	}
	return data
}

func parseMeta(out string) map[string]string {
	meta := make(map[string]string)
	for key, value := range parseExtradata(out) {
		if name, ok := strings.CutPrefix(key, metaPrefix); ok && name != "" {
			meta[name] = value
		}
	}
	return meta
}

var snapshotLine = regexp.MustCompile(`^(SnapshotName|SnapshotUUID|SnapshotDescription|CurrentSnapshotName|CurrentSnapshotUUID|CurrentSnapshotNode)((?:-\d+)*)="(.*)$`)

func parseSnapshots(out string) []vm.Snapshot {
	type node struct{ name, id, description string }
	nodes := make(map[string]*node)
	var order []string
	var current string
	var target *string
	for _, line := range lines(out) {
		m := snapshotLine.FindStringSubmatch(line)
		if m == nil {
			if target != nil {
				*target += "\n" + line
			}
			continue
		}
		kind, path, value := m[1], m[2], m[3]
		target = nil
		if kind == "CurrentSnapshotUUID" {
			current = strings.TrimSuffix(value, `"`)
			continue
		}
		if !strings.HasPrefix(kind, "Snapshot") {
			continue
		}
		n, ok := nodes[path]
		if !ok {
			n = &node{}
			nodes[path] = n
			order = append(order, path)
		}
		switch kind {
		case "SnapshotName":
			n.name = value
			target = &n.name
		case "SnapshotUUID":
			n.id = value
			target = &n.id
		case "SnapshotDescription":
			n.description = value
			target = &n.description
		}
	}
	snapshots := make([]vm.Snapshot, 0, len(order))
	for _, path := range order {
		n := nodes[path]
		s := vm.Snapshot{
			Name:        strings.TrimSuffix(n.name, `"`),
			ID:          strings.TrimSuffix(n.id, `"`),
			Description: strings.TrimSuffix(n.description, `"`),
		}
		if i := strings.LastIndex(path, "-"); i >= 0 {
			if parent, ok := nodes[path[:i]]; ok {
				s.Parent = strings.TrimSuffix(parent.name, `"`)
			}
		}
		s.Current = s.ID != "" && s.ID == current
		snapshots = append(snapshots, s)
	}
	return snapshots
}

func parseOSTypes(out string) map[string]string {
	types := make(map[string]string)
	for _, line := range lines(out) {
		rest, ok := strings.CutPrefix(line, "ID / Description: ")
		if !ok {
			continue
		}
		if id, description, ok := strings.Cut(rest, " -- "); ok {
			types[description] = id
		}
	}
	return types
}

func lineValue(out, prefix string) (string, bool) {
	for _, line := range lines(out) {
		if v, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

func parseMediumLocations(out string) []string {
	var locations []string
	for _, line := range lines(out) {
		if v, ok := strings.CutPrefix(line, "Location:"); ok {
			locations = append(locations, strings.TrimSpace(v))
		}
	}
	return locations
}

func parseCapacityMB(out string) int {
	v, _ := lineValue(out, "Capacity:")
	n, _ := strconv.Atoi(strings.TrimSuffix(v, " MBytes"))
	return n
}
