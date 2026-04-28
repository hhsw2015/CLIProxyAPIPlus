//go:build commercial

package commercial

import (
	"github.com/sirupsen/logrus"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// BridgeLogrusToZap adds a logrus hook that forwards all CPA log entries
// to sub2api's zap logger, unifying log output format.
func BridgeLogrusToZap(zapLogger *zap.Logger) {
	if zapLogger == nil {
		return
	}
	logrus.AddHook(&zapBridge{logger: zapLogger})
}

type zapBridge struct {
	logger *zap.Logger
}

func (h *zapBridge) Levels() []logrus.Level {
	return logrus.AllLevels
}

func (h *zapBridge) Fire(entry *logrus.Entry) error {
	fields := make([]zap.Field, 0, len(entry.Data))
	for k, v := range entry.Data {
		fields = append(fields, zap.Any(k, v))
	}

	var lvl zapcore.Level
	switch entry.Level {
	case logrus.TraceLevel, logrus.DebugLevel:
		lvl = zapcore.DebugLevel
	case logrus.InfoLevel:
		lvl = zapcore.InfoLevel
	case logrus.WarnLevel:
		lvl = zapcore.WarnLevel
	case logrus.ErrorLevel:
		lvl = zapcore.ErrorLevel
	case logrus.FatalLevel, logrus.PanicLevel:
		lvl = zapcore.FatalLevel
	default:
		lvl = zapcore.InfoLevel
	}

	if ce := h.logger.Check(lvl, entry.Message); ce != nil {
		ce.Write(fields...)
	}
	return nil
}
