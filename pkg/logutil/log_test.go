// Copyright 2017 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logutil

import (
	"bytes"
	"fmt"
	"testing"

	. "github.com/pingcap/check"
	zaplog "github.com/pingcap/log"
	log "github.com/sirupsen/logrus"
	"go.uber.org/zap/zapcore"
)

// const (
// 	logPattern = `\d\d\d\d/\d\d/\d\d \d\d:\d\d:\d\d\.\d\d\d ([\w_%!$@.,+~-]+|\\.)+:\d+: \[(fatal|error|warning|info|debug)\] .*?\n`
// )

func Test(t *testing.T) {
	TestingT(t)
}

var _ = Suite(&testLogSuite{})

type testLogSuite struct {
	buf *bytes.Buffer
}

func (s *testLogSuite) SetUpSuite(c *C) {
	s.buf = &bytes.Buffer{}
}

func (s *testLogSuite) TestStringToLogLevel(c *C) {
	c.Assert(StringToLogLevel("fatal"), Equals, log.FatalLevel)
	c.Assert(StringToLogLevel("ERROR"), Equals, log.ErrorLevel)
	c.Assert(StringToLogLevel("warn"), Equals, log.WarnLevel)
	c.Assert(StringToLogLevel("warning"), Equals, log.WarnLevel)
	c.Assert(StringToLogLevel("debug"), Equals, log.DebugLevel)
	c.Assert(StringToLogLevel("info"), Equals, log.InfoLevel)
	c.Assert(StringToLogLevel("whatever"), Equals, log.InfoLevel)
}

func (s *testLogSuite) TestStringToZapLogLevel(c *C) {
	c.Assert(StringToZapLogLevel("fatal"), Equals, zapcore.FatalLevel)
	c.Assert(StringToZapLogLevel("ERROR"), Equals, zapcore.ErrorLevel)
	c.Assert(StringToZapLogLevel("warn"), Equals, zapcore.WarnLevel)
	c.Assert(StringToZapLogLevel("warning"), Equals, zapcore.WarnLevel)
	c.Assert(StringToZapLogLevel("debug"), Equals, zapcore.DebugLevel)
	c.Assert(StringToZapLogLevel("info"), Equals, zapcore.InfoLevel)
	c.Assert(StringToZapLogLevel("whatever"), Equals, zapcore.InfoLevel)
}

func (s *testLogSuite) TestStringToLogFormatter(c *C) {
	c.Assert(StringToLogFormatter("text", true), DeepEquals, &textFormatter{
		DisableTimestamp: true,
	})
	c.Assert(StringToLogFormatter("json", true), DeepEquals, &log.JSONFormatter{
		DisableTimestamp: true,
		TimestampFormat:  defaultLogTimeFormat,
	})
	c.Assert(StringToLogFormatter("console", true), DeepEquals, &log.TextFormatter{
		DisableTimestamp: true,
		FullTimestamp:    true,
		TimestampFormat:  defaultLogTimeFormat,
	})
	c.Assert(StringToLogFormatter("", true), DeepEquals, &textFormatter{})
}

// TestLogging assure log format and log redirection works.
func (s *testLogSuite) TestLogging(c *C) {
	conf := &zaplog.Config{Level: "warn", File: zaplog.FileLogConfig{}}
	c.Assert(InitLogger(conf), IsNil)

	log.SetOutput(s.buf)
	// Skip capnslog temporarily
	// tlog := capnslog.NewPackageLogger("github.com/tikv/pd/pkg/logutil", "test")

	// tlog.Infof("[this message should not be sent to buf]")
	// c.Assert(s.buf.Len(), Equals, 0)

	// tlog.Warningf("[this message should be sent to buf]")
	// entry, err := s.buf.ReadString('\n')
	// c.Assert(err, IsNil)
	// c.Assert(entry, Matches, logPattern)
	// All capnslog log will be trigered in logutil/log.go
	// c.Assert(strings.Contains(entry, "log.go"), IsTrue)

	// log.Warnf("this message comes from logrus")
	// entry, err := s.buf.ReadString('\n')
	// c.Assert(err, IsNil)
	// c.Assert(entry, Matches, logPattern)
	// c.Assert(strings.Contains(entry, "log_test.go"), IsTrue)
}

func (s *testLogSuite) TestFileLog(c *C) {
	c.Assert(InitFileLog(&zaplog.FileLogConfig{Filename: "/tmp"}), NotNil)
	c.Assert(InitFileLog(&zaplog.FileLogConfig{Filename: "/tmp/test_file_log", MaxSize: 0}), IsNil)
}

func (s *testLogSuite) TestRedactLog(c *C) {
	testcases := []struct {
		name            string
		arg             interface{}
		enableRedactLog bool
		expect          interface{}
	}{
		{
			name:            "string arg, enable redact",
			arg:             "foo",
			enableRedactLog: true,
			expect:          "?",
		},
		{
			name:            "string arg",
			arg:             "foo",
			enableRedactLog: false,
			expect:          "foo",
		},
		{
			name:            "[]byte arg, enable redact",
			arg:             []byte("foo"),
			enableRedactLog: true,
			expect:          []byte("?"),
		},
		{
			name:            "[]byte arg",
			arg:             []byte("foo"),
			enableRedactLog: false,
			expect:          []byte("foo"),
		},
	}

	for _, testcase := range testcases {
		c.Log(testcase.name)
		SetRedactLog(testcase.enableRedactLog)
		switch r := testcase.arg.(type) {
		case []byte:
			c.Assert(RedactBytes(r), DeepEquals, testcase.expect)
		case string:
			c.Assert(RedactString(r), DeepEquals, testcase.expect)
		case fmt.Stringer:
			c.Assert(RedactStringer(r), DeepEquals, testcase.expect)
		default:
			panic("unmatched case")
		}
	}
}
