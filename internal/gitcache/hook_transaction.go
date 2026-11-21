package gitcache

import (
	"bufio"
	"context"
	"io"
	"strings"
)

// Git supplies the exact transaction on stdin, including an optional HEAD update.
func readHookUpdates(input io.Reader) ([]RefUpdate, error) {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 4096)
	var updates []RefUpdate
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			return nil, refuse("receive update")
		}
		if len(updates) >= MaxUpdates+1 {
			return nil, refuse("receive update count")
		}
		updates = append(updates, RefUpdate{OldSHA: zeroEmpty(fields[0]), NewSHA: zeroEmpty(fields[1]), Ref: fields[2]})
	}
	return updates, scanner.Err()
}

func callTransactionGate(ctx context.Context, phase string, input io.Reader) error {
	request := gateRequest{Phase: phase}
	if phase == "prepared" {
		var err error
		request.Updates, err = readHookUpdates(input)
		if err != nil {
			return err
		}
	}
	return callGateRequest(ctx, request)
}
