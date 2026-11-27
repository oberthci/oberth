package installer

import (
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"

	"k8s.io/apimachinery/pkg/util/httpstream"
)

type failedForwardStream struct {
	httpstream.Stream
	err error
}

func (s failedForwardStream) Write([]byte) (int, error) { return 0, s.err }

func TestForwardingBrokenPipeIsOrdinaryClosure(t *testing.T) {
	for _, cause := range []error{syscall.EPIPE, &net.OpError{Op: "write", Net: "tcp", Err: syscall.EPIPE}} {
		stream := forwardingStream{failedForwardStream{err: cause}}
		_, err := stream.Write([]byte("request"))
		// client-go's forwarder excludes this closure text from HandleError.
		if !errors.Is(err, net.ErrClosed) || !strings.Contains(strings.ToLower(err.Error()), "use of closed network connection") {
			t.Fatal("broken pipe reaches native unhandled-error log", err)
		}
	}
	cause := errors.New("stream rejected")
	_, err := (forwardingStream{failedForwardStream{err: cause}}).Write(nil)
	if !errors.Is(err, cause) {
		t.Fatal("unexpected forwarding error hidden", err)
	}
}
