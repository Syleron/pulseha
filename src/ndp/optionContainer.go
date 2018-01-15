package ndp

/**

 */
type optionContainer struct {
	Options ICMPOptions
}

/**

 */
func (oc *optionContainer) AddOption(o ICMPOption) {
	oc.Options = append(oc.Options, o)
}

/**
HasOption returns true if ICMP contains option of type ICMPOptionType
 */
func (oc optionContainer) HasOption(t ICMPOptionType) bool {
	for _, o := range oc.Options {
		if o.Type() == t {
			return true
		}
	}
	return false
}

/**

 */
func parseOptions() {

}
