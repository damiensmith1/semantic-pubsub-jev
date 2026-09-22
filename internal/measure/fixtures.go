// Package measure runs the experiments this project exists to produce.
//
// The fixtures here are the experiment's independent variable, so they
// are deliberate rather than convenient. A stability run over obviously
// separable interests would report near-perfect agreement and tell you
// nothing: the interesting behaviour is where a message is arguably
// relevant, because that is where a probability sits near the threshold
// and a small wobble changes the routing decision.
package measure

// Interest is one subscriber's stated predicate.
type Interest struct {
	ID        string
	Predicate string
}

// Message is one published payload.
type Message struct {
	ID   string
	Body map[string]any
}

// Case pairs a message with an expectation, where one exists.
//
// Expect is what a careful person would say the answer should be.
// Borderline cases have no expectation on purpose — the point is not
// whether the model agrees with me, but whether it agrees with *itself*
// across runs.
type Case struct {
	MessageID  string
	InterestID string
	Expect     Expectation
}

// Expectation is the intended answer for a case.
type Expectation int

const (
	// Borderline means reasonable people could disagree. Stability still
	// matters; correctness is not asserted.
	Borderline Expectation = iota
	ShouldDeliver
	ShouldNotDeliver
)

func (e Expectation) String() string {
	switch e {
	case ShouldDeliver:
		return "deliver"
	case ShouldNotDeliver:
		return "skip"
	default:
		return "borderline"
	}
}

// Interests are the standing subscriptions used across experiments.
var Interests = []Interest{
	{"storage", "database and storage problems, including disk capacity"},
	{"network", "network latency and connectivity issues"},
	{"security", "security incidents and authentication failures"},
	{"outages", "hard outages where a service is completely unavailable"},
	{"customer", "anything that customers would notice or complain about"},
	{"eu", "anything affecting the eu-west region"},
}

// Messages span the clean cases and the awkward ones.
var Messages = []Message{
	// Clear-cut: these should be stable at the extremes, and if they are
	// not, nothing else in the experiment matters.
	{"disk-full", map[string]any{
		"service": "orders-db", "severity": "critical", "region": "us-east-1",
		"text": "primary database volume at 96% capacity, writes will fail within the hour",
	}},
	{"latency", map[string]any{
		"service": "edge-proxy", "severity": "warning", "region": "eu-west-1",
		"text": "p99 round-trip time up 400ms, packet loss 2% on the eu-west edge",
	}},
	{"bruteforce", map[string]any{
		"service": "auth-api", "severity": "critical", "region": "us-east-1",
		"text": "4,000 failed login attempts from a single ASN in 10 minutes",
	}},

	// The near miss. A deploy notice that names the same service a
	// subscriber watches for outages. Topical similarity would deliver
	// it; a delivery question should not. This is the case the question
	// wording was designed around, and it has never been tested.
	{"deploy-mentions-checkout", map[string]any{
		"service": "checkout-api", "severity": "info", "region": "us-east-1",
		"text": "deployed checkout-api v4.2.1, all health checks green, no downtime",
	}},

	// Genuinely ambiguous: degraded but not down, customer-affecting but
	// not an outage. Expect probabilities near the middle.
	{"partial-degradation", map[string]any{
		"service": "checkout-api", "severity": "warning", "region": "us-east-1",
		"text": "checkout succeeding but 8% slower than baseline; no errors reported",
	}},

	// A storage problem that is also an outage and also customer-facing.
	// Several interests should match at once, which a Choice primitive
	// could not express.
	{"cascade", map[string]any{
		"service": "orders-db", "severity": "critical", "region": "eu-west-1",
		"text": "database failover failed, orders service returning 503 to all customers",
	}},

	// Region mismatch: a subscriber watching eu-west against a us-east
	// alert. Catches a judge keying on topic or service rather than the
	// message content.
	{"us-east-outage", map[string]any{
		"service": "payments", "severity": "critical", "region": "us-east-1",
		"text": "payments service completely unavailable in us-east-1 for 12 minutes",
	}},
}

// Cases are the pairs evaluated for stability, with expectations where a
// careful person would be confident.
var Cases = []Case{
	{"disk-full", "storage", ShouldDeliver},
	{"disk-full", "network", ShouldNotDeliver},
	{"disk-full", "eu", ShouldNotDeliver},
	{"disk-full", "customer", Borderline},

	{"latency", "network", ShouldDeliver},
	{"latency", "eu", ShouldDeliver},
	{"latency", "storage", ShouldNotDeliver},
	{"latency", "customer", Borderline},

	{"bruteforce", "security", ShouldDeliver},
	{"bruteforce", "storage", ShouldNotDeliver},

	// The near miss and the ambiguous middle: no expectation, because
	// what is being measured is self-consistency.
	{"deploy-mentions-checkout", "outages", ShouldNotDeliver},
	{"deploy-mentions-checkout", "customer", Borderline},
	{"partial-degradation", "outages", Borderline},
	{"partial-degradation", "customer", Borderline},

	{"cascade", "storage", ShouldDeliver},
	{"cascade", "outages", ShouldDeliver},
	{"cascade", "customer", ShouldDeliver},
	{"cascade", "eu", ShouldDeliver},

	{"us-east-outage", "outages", ShouldDeliver},
	{"us-east-outage", "eu", ShouldNotDeliver},
}

// MessageByID and InterestByID index the fixtures.
func MessageByID(id string) (Message, bool) {
	for _, m := range Messages {
		if m.ID == id {
			return m, true
		}
	}
	return Message{}, false
}

func InterestByID(id string) (Interest, bool) {
	for _, i := range Interests {
		if i.ID == id {
			return i, true
		}
	}
	return Interest{}, false
}
