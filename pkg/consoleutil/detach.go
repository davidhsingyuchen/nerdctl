/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package consoleutil

import (
	"errors"
	"fmt"
	"io"

	"github.com/moby/term"
	"github.com/sirupsen/logrus"
)

const DefaultDetachKeys = "ctrl-p,ctrl-q"

type detachableStdin struct {
	stdin  io.Reader
	closer func()
}

// NewDetachableStdin returns a io.Reader that
// uses a TTY proxy reader to read from stdin and detect when the specified detach keys are read,
// in which case closer will be called.
func NewDetachableStdin(stdin io.Reader, keys string, closer func()) (io.Reader, error) {
	if len(keys) == 0 {
		keys = DefaultDetachKeys
	}
	b, err := term.ToBytes(keys)
	if err != nil {
		return nil, fmt.Errorf("failed to convert the detach keys to bytes: %w", err)
	}
	return &detachableStdin{
		stdin:  term.NewEscapeProxy(stdin, b),
		closer: closer,
	}, nil
}

func (ds *detachableStdin) Read(p []byte) (int, error) {
	n, err := ds.stdin.Read(p)
	var eerr term.EscapeError
	if errors.As(err, &eerr) {
		logrus.Info("read detach keys")
		if ds.closer != nil {
			ds.closer()
		}
		// Technically we should return a nil error here as the "error" is expected, but if we do so,
		// `nr, er := src.Read` in the for loop [1] (invoked from [2]) will have nr = 0 and er = nil,
		// which means that the loop becomes an infinite loop,
		// and it will only "exits" when IO.Wait() of the current task returns,
		// as the whole nerdctl process will exit after that.
		//
		// The implication is that "read detach keys" will be printed once for every iteration of the loop,
		// while we only want it to be printed a single time,
		// so we just return a non-nil error here to make things simpler.
		//
		// [1] https://github.com/golang/go/blob/c0fd7f79fe445ad49e11bf42c8c785cb71b3bedf/src/io/io.go#L426-L452
		// [2] https://github.com/containerd/containerd/blob/8f756bc8c26465bd93e78d9cd42082b66f276e10/cio/io_unix.go#L68
		return n, err
	}
	return n, err
}
