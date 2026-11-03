package desktop

// What a start that does not want KSM does about it.
const (
	ksmLeave   = iota // it is off, or on and not Decurix's: nothing to do
	ksmTurnOff        // Decurix turned it on, or the person asked: turn it off
	ksmSayOn          // on, not Decurix's doing: say so, leave it
)

// ksmOffDecision decides, for a start that does not want KSM: explicit is the person's DECURIX_DISABLE_KSM (or
// DECURIX_ENABLE_KSM=0), on whether KSM runs, marked whether Decurix's own marker says it turned it on.
func ksmOffDecision(explicit, on, marked bool) int {
	switch {
	case !on:
		return ksmLeave
	case explicit || marked:
		return ksmTurnOff
	default:
		return ksmSayOn
	}
}
