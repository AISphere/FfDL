/*
 * Copyright 2017-2018 IBM Corporation
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */


package instrumentation

import (
	"fmt"
	"math/rand"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"

	"github.com/AISphere/ffdl-commons/logger"
)

// CallLogger is used to log the entry and exit of a function, including whether a context has been completed.
// It can also be used to observe any interesting events in between.
type CallLogger interface {
	// Observe some intermediate point in the function call.
	Observe(string, ...interface{})

	// Call this when the function returns. Usually use in a defer call.
	Returned()
}

type callLogger struct {
	id           string // A unique id for this call
	functionName string
	logr         *logger.LocLoggingEntry
	start        time.Time
	deadline     *time.Time
	returned     bool
}

// NewCallLogger creates a new logger for a function with an optional context.
func NewCallLogger(ctx context.Context, functionName string, logr *logger.LocLoggingEntry) CallLogger {

	now := time.Now()
	// Use a local logger if one isn't supplied.
	if logr == nil {
		logr = logger.LocLogger(log.StandardLogger().WithField("module", "CallLogger"))
	}

	id := fmt.Sprintf("%d-%04d", now.UnixNano(), rand.Intn(10000))
	l := &callLogger{
		id:           id,
		functionName: functionName,
		logr:         logr,
		start:        now,
		returned:     false,
	}

	// Record when the context has expired.
	if ctx != nil {
		done := ctx.Done()
		deadline, _ := ctx.Deadline()
		l.deadline = &deadline
		go func() {
			_ = <-done
			now := time.Now()
			l.logr.Debugf("%s context done returned=%v, reason: %s %s", l.prefixStr(now), l.returned, ctx.Err(), l.deadlineStr(now))
		}()
	}

	var ctxErr error
	if ctx != nil {
		ctxErr = ctx.Err()
	}
	l.logr.Debugf("%s enter function %s, context status: %s", l.prefixStr(now), l.deadlineStr(now), ctxErr)

	return l
}

func (l *callLogger) Observe(format string, args ...interface{}) {
	now := time.Now()
	msg := fmt.Sprintf(format, args...)
	l.logr.Debugf("%s %s", l.prefixStr(now), msg)
}

func (l *callLogger) Returned() {
	l.returned = true
	now := time.Now()
	l.logr.Debugf("%s exit function %s", l.prefixStr(now), l.deadlineStr(now))
}

func (l *callLogger) prefixStr(now time.Time) string {
	return fmt.Sprintf("[%s %s at %v +%v]", l.id, l.functionName, now.UTC(), now.Sub(l.start))
}

func (l *callLogger) deadlineStr(now time.Time) string {
	deadlineStr := ""
	if l.deadline != nil {
		deadlineStr = fmt.Sprintf("(%v before deadline)", l.deadline.Sub(now))
	}
	return deadlineStr
}
