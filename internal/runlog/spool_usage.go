package runlog

import (
	"os"
	"sync/atomic"

	"github.com/oberthci/oberth/internal/logcontract"
)

type spoolState struct{ unlinked atomic.Bool }

func (s *Store) closeSpool(file *os.File) {
	// Keep the file charged until close releases its blocks.
	_ = file.Close()
	s.spools.Delete(file)
}

func (s *Store) spoolUsage(out *logcontract.StoragePreview) {
	s.spools.Range(func(raw, state any) bool {
		if !state.(*spoolState).unlinked.Load() {
			out.Complete = false
			return true
		}
		info, err := raw.(*os.File).Stat()
		if err != nil {
			out.Complete = false
			return true
		}
		size, known := storageBytes(info)
		out.Complete = out.Complete && known
		out.OpenSpoolCount++
		addBytes(&out.OpenSpools, size)
		return true
	})
}
