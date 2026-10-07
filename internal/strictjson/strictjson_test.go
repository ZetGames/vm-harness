package strictjson

import (
	"errors"
	"strings"
	"testing"

	"github.com/fl4metf/vm-harness/vm"
)

func TestDecode(t *testing.T) {
	type spec struct {
		Name string `json:"name"`
	}
	cases := []struct {
		in string
		ok bool
	}{
		{`{"name":"web"}`, true},
		{"  {\"name\":\"web\"}\n\n", true},
		{`{"name":"web"}}`, false},
		{`{"name":"web"}]`, false},
		{`{"name":"web"} {"name":"db"}`, false},
		{`{"name":"web","cpus":2}`, false},
		{``, false},
		{`{"name":`, false},
	}
	for _, c := range cases {
		var s spec
		err := Decode(strings.NewReader(c.in), &s)
		if (err == nil) != c.ok {
			t.Errorf("Decode(%q) err = %v, want ok=%v", c.in, err, c.ok)
		}
		if err != nil && !errors.Is(err, vm.ErrInvalid) {
			t.Errorf("Decode(%q) error %v does not wrap ErrInvalid", c.in, err)
		}
	}
}
