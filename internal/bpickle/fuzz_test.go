package bpickle

import (
	"bytes"
	"testing"
)

// FuzzUnmarshal asserts the decoder never panics and never loops forever on
// arbitrary input. It parses untrusted server responses, including from the
// plain-http ping endpoint.
func FuzzUnmarshal(f *testing.F) {
	f.Add([]byte("i42;"))
	f.Add([]byte("u5:hello"))
	f.Add([]byte("s5:hello"))
	f.Add([]byte("b1"))
	f.Add([]byte("f3.14;"))
	f.Add([]byte("n"))
	f.Add([]byte("li1;i2;i3;;"))
	f.Add([]byte("ti1;i2;;"))
	f.Add([]byte("du4:types li1;;;"))
	f.Add([]byte("du4:types lu3:foou3:bar;;"))
	f.Add(append(bytes.Repeat([]byte{'l'}, 200), bytes.Repeat([]byte{';'}, 200)...))
	f.Add([]byte("l"))
	f.Add([]byte("s99:short"))
	f.Add([]byte("i;"))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Only requirement: return a value or an error, never panic.
		_, _ = Unmarshal(data)
	})
}
