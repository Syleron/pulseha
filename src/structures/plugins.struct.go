package structures

type PluginHC interface {
	Name() string
	Version() float64
	Send() (bool, bool)
}