package message

import (
	"context"
	"regexp"
	"slices"
	"strconv"

	"github.com/google/uuid"

	"github.com/eastsidekillas/ruscord-core/internal/perm"
	"github.com/eastsidekillas/ruscord-core/internal/platform/httpx"
)

// maxMentions caps the distinct users and roles one message may mention, so
// a single message cannot ping a whole server one by one.
const maxMentions = 50

const uuidPattern = `([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`

var (
	everyoneRegex = regexp.MustCompile(`@(everyone|here)\b`)
	userMention   = regexp.MustCompile(`<@` + uuidPattern + `>`)
	roleMention   = regexp.MustCompile(`<@&` + uuidPattern + `>`)
	// codeSpan matches code blocks and inline code, which clients show as
	// text: a mention inside them pings nobody. Blocks may span lines,
	// inline code may not. ruscord-fe's markup parser follows the same rules
	// in the same order (src/shared/lib/markup.ts).
	codeSpan = regexp.MustCompile("(?s:```.*?```)|``[^\n]+?``|`[^`\n]+`")
)

// parsed is what the text of a message mentions before the ids are checked
// against the channel.
type parsed struct {
	everyone bool
	users    []uuid.UUID
	roles    []uuid.UUID
}

func parseMentions(content string) (parsed, error) {
	text := codeSpan.ReplaceAllString(content, " ")
	p := parsed{
		everyone: everyoneRegex.MatchString(text),
		users:    idsIn(userMention, text),
		roles:    idsIn(roleMention, text),
	}
	if len(p.users)+len(p.roles) > maxMentions {
		return parsed{}, httpx.Validation(map[string]string{"content": "Не больше " + strconv.Itoa(maxMentions) + " упоминаний в сообщении"})
	}
	return p, nil
}

// idsIn returns the distinct ids the pattern finds, in the order of the text.
func idsIn(re *regexp.Regexp, text string) []uuid.UUID {
	out := []uuid.UUID{}
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		id, err := uuid.Parse(m[1])
		if err == nil && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// mentions is what a message pings after checking.
type mentions struct {
	everyone bool
	users    []uuid.UUID
	roles    []uuid.UUID
}

func (m mentions) apply(msg *Message) {
	msg.MentionEveryone, msg.Mentions, msg.MentionRoles = m.everyone, m.users, m.roles
}

// resolveMentions checks parsed mentions against ch. In a guild channel
// users must be members and roles must belong to the guild and be pingable
// by the author; in a DM only its recipients can be pinged, and roles do not
// exist. replyAuthor, when set, is pinged as the author of the message the
// reply answers.
func (s *Service) resolveMentions(ctx context.Context, ch channel, p parsed, replyAuthor *uuid.UUID) (mentions, error) {
	users := p.users
	if replyAuthor != nil && !slices.Contains(users, *replyAuthor) {
		users = append(slices.Clone(users), *replyAuthor)
	}
	pingAll := ch.perms.Has(perm.MentionEveryone)
	out := mentions{everyone: p.everyone && pingAll, users: []uuid.UUID{}, roles: []uuid.UUID{}}
	if ch.guildID == nil {
		out.users = slices.DeleteFunc(users, func(id uuid.UUID) bool { return !slices.Contains(ch.recipients, id) })
		return out, nil
	}
	var err error
	out.users, out.roles, err = s.guilds.Mentions(ctx, *ch.guildID, users, p.roles, pingAll)
	return out, err
}
