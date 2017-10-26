package docker

// Logger is the interface to implement if you want log message to be written during the docker lifecycle.
type Logger interface {
	Print(v ...interface{})
	Printf(format string, v ...interface{})
}

type defaultLogger struct{}

func (l defaultLogger) Print(v ...interface{})                {}
func (l defaultLogger) Printf(fomat string, v ...interface{}) {}
