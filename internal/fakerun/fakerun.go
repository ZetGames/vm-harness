package fakerun

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/ZetGames/vm-harness/runner"
)

type Call struct {
	Name string
	Args []string
}

func (c Call) String() string { return strings.Join(c.Args, " ") }

type rule struct {
	prefix  string
	results []runner.Result
	errs    []error
	fn      func(Call) (runner.Result, error)
	used    int
}

type Fake struct {
	mu    sync.Mutex
	rules []*rule
	calls []Call
}

func New() *Fake { return &Fake{} }

func (f *Fake) On(prefix, stdout string) *Fake {
	return f.OnResult(prefix, runner.Result{Stdout: []byte(stdout)})
}

func (f *Fake) OnResult(prefix string, results ...runner.Result) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, &rule{prefix: prefix, results: results})
	return f
}

func (f *Fake) OnError(prefix string, err error) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, &rule{prefix: prefix, results: []runner.Result{{}}, errs: []error{err}})
	return f
}

func (f *Fake) OnFunc(prefix string, fn func(Call) (runner.Result, error)) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append(f.rules, &rule{prefix: prefix, fn: fn})
	return f
}

func (f *Fake) Run(ctx context.Context, name string, args ...string) (runner.Result, error) {
	if err := ctx.Err(); err != nil {
		return runner.Result{}, err
	}
	call := Call{Name: name, Args: append([]string(nil), args...)}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	r := f.match(call.String())
	if r == nil {
		f.mu.Unlock()
		return runner.Result{}, fmt.Errorf("fakerun: unexpected command: %s %s", name, call)
	}
	if r.fn != nil {
		fn := r.fn
		f.mu.Unlock()
		return fn(call)
	}
	i := min(r.used, len(r.results)-1)
	r.used++
	res := r.results[i]
	var err error
	if i < len(r.errs) {
		err = r.errs[i]
	}
	f.mu.Unlock()
	return res, err
}

func (f *Fake) match(line string) *rule {
	for i := len(f.rules) - 1; i >= 0; i-- {
		if strings.HasPrefix(line, f.rules[i].prefix) {
			return f.rules[i]
		}
	}
	return nil
}

func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

func (f *Fake) Called(prefix string) bool {
	for _, c := range f.Calls() {
		if strings.HasPrefix(c.String(), prefix) {
			return true
		}
	}
	return false
}

func (f *Fake) Find(prefix string) []Call {
	var out []Call
	for _, c := range f.Calls() {
		if strings.HasPrefix(c.String(), prefix) {
			out = append(out, c)
		}
	}
	return out
}
