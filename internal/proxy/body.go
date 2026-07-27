package proxy

import (
	"fmt"
	"io"
)

const (
	maxMutationRequestBodyBytes  = 16 << 20
	maxUpstreamResponseBodyBytes = 64 << 20
)

type bodyLimitError struct {
	description string
	limit       int64
}

func (e *bodyLimitError) Error() string {
	return fmt.Sprintf("%s exceeds %d byte limit", e.description, e.limit)
}

func readLimitedBody(reader io.Reader, limit int64, description string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, &bodyLimitError{description: description, limit: limit}
	}
	return data, nil
}
