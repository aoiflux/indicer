package ui

// fortune is one line for `dues fortune`.
type fortune struct {
	text, source string
}

// fortunes are lines about the craft: public-domain Sherlock Holmes, with
// story and year, and lines of DUES's own. The rule for adding one: humour
// about the work (hashes, timestamps, coffee) only, never about crimes,
// victims, suspects, courts or agencies.
var fortunes = []fortune{
	{"It is a capital mistake to theorize before one has data.", "Sherlock Holmes, A Scandal in Bohemia (1891)"},
	{"You see, but you do not observe.", "Sherlock Holmes, A Scandal in Bohemia (1891)"},
	{"Data! data! data! I can't make bricks without clay.", "Sherlock Holmes, The Adventure of the Copper Beeches (1892)"},
	{"There is nothing more deceptive than an obvious fact.", "Sherlock Holmes, The Boscombe Valley Mystery (1891)"},
	{"It has long been an axiom of mine that the little things are infinitely the most important.", "Sherlock Holmes, A Case of Identity (1891)"},
	{"The world is full of obvious things which nobody by any chance ever observes.", "Sherlock Holmes, The Hound of the Baskervilles (1902)"},
	{"I never guess. It is a shocking habit, destructive to the logical faculty.", "Sherlock Holmes, The Sign of the Four (1890)"},
	{"When you have eliminated the impossible, whatever remains, however improbable, must be the truth.", "Sherlock Holmes, The Sign of the Four (1890)"},
	{"There is nothing like first-hand evidence.", "Sherlock Holmes, A Study in Scarlet (1887)"},
	{"Hash first. Ask questions later.", ""},
	{"Timestamps lie, logs gossip, hashes testify.", ""},
	{"If it isn't documented, it didn't happen.", ""},
	{"Deleted is a state of mind.", ""},
	{"Unallocated space remembers.", ""},
	{"Trust, but verify the hash.", ""},
	{"The chain of custody is the only chain worth keeping.", ""},
	{"Dedup is evidence admitting it has said this before.", ""},
	{"Same bytes, same hash, same story.", ""},
	{"Read-only is a love language.", ""},
	{"Running low? dues coffee checks the examiner's caffeine levels.", ""},
}
