package strictjson

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/ZetGames/vm-harness/vm"
)

func Decode(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %w", vm.ErrInvalid, err)
	}
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: unexpected data after the JSON value", vm.ErrInvalid)
	}
	return nil
}
