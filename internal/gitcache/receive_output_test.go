package gitcache

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestReceiveOutputPreservesSplitProtocolPackets(t *testing.T) {
	packet := func(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
	advertisement := packet(strings.Repeat("a", 40) + " refs/heads/main\x00report-status side-band-64k\n")
	status := packet("\x01" + packet("unpack ok\n") + packet("ok refs/heads/topic\n") + "0000")
	stream := advertisement + "0000" + packet("\x02native progress\n") + packet("\x03native failure\n") + status + "0000"
	for _, chunk := range []int{1, 3, 17, len(stream)} {
		var result bytes.Buffer
		w := &receiveOutput{next: &result}
		for left := stream; left != ""; {
			n := min(chunk, len(left))
			if written, err := w.Write([]byte(left[:n])); err != nil || written != n {
				t.Fatal("partial native output failed", written, err)
			}
			left = left[n:]
		}
		if result.String() != advertisement+"0000"+status+"0000" || w.detail.String() != "native progress\nnative failure\n" || len(w.frame) != 0 {
			t.Fatal("protocol or diagnostics changed", chunk, result.String(), w.detail.String())
		}
	}
}
