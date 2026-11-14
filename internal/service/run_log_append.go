package service

type runLogAppender struct {
	store LogStore
	runID string
}

func (writer runLogAppender) Write(body []byte) (int, error) {
	return writer.store.AppendRunLog(writer.runID, body)
}
