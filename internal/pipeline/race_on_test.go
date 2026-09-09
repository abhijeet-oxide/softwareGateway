//go:build race

package pipeline_test

// raceEnabled says whether this binary was built with -race, which is the
// difference between a budget that means something and one that fires on
// instrumentation overhead. There is no way to ask the runtime, so the build
// tag answers it. See TestResolutionStaysCheapEnoughForAPageRender.
const raceEnabled = true
