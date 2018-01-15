package ndp

type RouterPreferenceField int

/**
types currently defined
 */
const (
	RouterPreferenceMedium RouterPreferenceField = iota
	RouterPreferenceHigh
	_
	RouterPreferenceLow
)

/**

 */
func (typ RouterPreferenceField) String() string {
	switch typ {
	case RouterPreferenceLow:
		return "low"
	case RouterPreferenceMedium:
		return "medium"
	case RouterPreferenceHigh:
		return "high"
	default:
		return "<nil>"
	}
}