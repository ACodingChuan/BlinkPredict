package logging

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
)

type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var currentLevel atomic.Int32

func init() {
	Configure("info")
}

func Configure(level string) {
	currentLevel.Store(int32(parseLevel(level)))
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.LUTC)
}

type Logger struct {
	component string
}

func New(component string) *Logger {
	return &Logger{component: component}
}

func (l *Logger) Debugf(format string, args ...any) {
	l.logf(LevelDebug, format, args...)
}

func (l *Logger) Infof(format string, args ...any) {
	l.logf(LevelInfo, format, args...)
}

func (l *Logger) Warnf(format string, args ...any) {
	l.logf(LevelWarn, format, args...)
}

func (l *Logger) Errorf(format string, args ...any) {
	l.logf(LevelError, format, args...)
}

func (l *Logger) Fatalf(format string, args ...any) {
	log.Fatalf("level=FATAL component=%s %s", l.component, fmt.Sprintf(format, args...))
}

func (l *Logger) logf(level Level, format string, args ...any) {
	if level < Level(currentLevel.Load()) {
		return
	}
	log.Printf("level=%s component=%s %s", levelString(level), l.component, fmt.Sprintf(format, args...))
}

func parseLevel(raw string) Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

func levelString(level Level) string {
	switch level {
	case LevelDebug:
		return "DEBUG"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}
