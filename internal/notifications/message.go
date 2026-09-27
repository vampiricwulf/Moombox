package notifications

// Embed is one embed's worth of a message: exactly what Manager.Send used to
// hand a sender as five positional arguments. Opts is per embed because
// Discord's title URL, author line, thumbnail, image and footer are all
// per-embed fields — a batch of ten `found`s is ten different jobs.
//
// Nothing writes an Embed after One (or the batcher) builds one, which is what
// makes sharing the slice across a Message's per-target copies safe.
type Embed struct {
	Title       string
	Description string
	Color       int
	Fields      []Field
	Opts        SendOptions
}

// Message is one Discord webhook POST: its embeds, and the single content
// mention that applies to the whole message. The mention is message-level
// because Discord's `content` and `allowed_mentions` are message-level — an
// embed can never ping anyone — so a batch pings once however many embeds it
// carries. SendOptions declares no mention at all: Manager.Send resolves the
// ping per target and writes it HERE, and setting one on an embed's Opts
// would ping nobody.
//
// Two per-message bounds hold, both enforced by splitMessages (batch.go) as it
// chops a flushed window: at most maxEmbedsPerMessage (Discord's ten, an
// eleventh being a permanent 400 for the WHOLE message), and at most
// limitTotal characters summed over the CLAMPED embeds — the same conversion
// buildPayload uses, so what the splitter measures is what goes on the wire.
// One is one embed by construction; everything larger comes from the batcher.
type Message struct {
	Embeds         []Embed
	Mention        string
	MentionAllowed *AllowedMentions
}

// One is the single-embed Message every non-batched send is — the shape all
// ~36 producer sites still produce, byte for byte.
//
// fields is stored BY REFERENCE — the caller owns the copy. Manager.Send makes
// it (one allocation per send) precisely because the usual caller hands over a
// FieldBuilder buffer it is free to reuse, and a queued item can now sit for
// seconds. A new caller of One that passes a live buffer gets aliasing.
func One(title, description string, color int, fields []Field, opts SendOptions) Message {
	return Message{Embeds: []Embed{{
		Title:       title,
		Description: description,
		Color:       color,
		Fields:      fields,
		Opts:        opts,
	}}}
}

// logEvent and logTitle name a message for a log line: the first embed's,
// which for a single-embed message is the only one. An empty Message answers
// "" rather than panicking — a log line is the last place an index panic
// should come from.
func (m Message) logEvent() string {
	if len(m.Embeds) == 0 {
		return ""
	}
	return m.Embeds[0].Opts.Event
}

func (m Message) logTitle() string {
	if len(m.Embeds) == 0 {
		return ""
	}
	return m.Embeds[0].Title
}
