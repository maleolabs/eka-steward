package obs

// Composite fans the loop's observability events out to each observer in
// order. Wire it as loop.Config.Observer so the Recorder (metrics + report) and
// the Stream (live events) both see every step and write, without either
// knowing about the other.
type Composite []Observer

// Step reports one step to each observer in order.
func (c Composite) Step(e StepEvent) {
	for _, o := range c {
		o.Step(e)
	}
}

// Write reports one EKA write to each observer in order.
func (c Composite) Write(e WriteEvent) {
	for _, o := range c {
		o.Write(e)
	}
}
