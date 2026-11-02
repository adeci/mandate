package console

import (
	"context"
	"maps"
	"net/http"
	"regexp"
	"runtime/debug"
	"slices"
	"time"

	"github.com/adeci/mandate/connector"
)

// release is the tag this build was released as, set with
// -ldflags "-X github.com/adeci/mandate/internal/console.release=vX.Y.Z".
// A build of a tagged module by go install carries it in its build info.
var release string

var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+(-rc\.\d+)?$`)

// sources says where agents get the CLI and the skill: from this build's
// release when it is one, else the latest release and main.
func sources() (releaseURL, skillURL string) {
	const repo = "https://github.com/adeci/mandate"
	tag := release
	if info, ok := debug.ReadBuildInfo(); ok && tag == "" {
		tag = info.Main.Version
	}
	if !releaseTag.MatchString(tag) {
		return repo + "/releases/latest/download", repo + "/blob/main/skills/mandate/SKILL.md"
	}
	return repo + "/releases/download/" + tag, repo + "/blob/" + tag + "/skills/mandate/SKILL.md"
}

// agentsData is the Agents screen; the page renders it and /agents.json
// serves it.
type agentsData struct {
	Agents []agentItem `json:"agents"`
	// GatewayURL, ReleaseURL and SkillURL fill a new agent's setup sheet.
	GatewayURL string `json:"gateway_url,omitempty"`
	ReleaseURL string `json:"release_url"`
	SkillURL   string `json:"skill_url"`
	CanSign    bool   `json:"can_sign"`
	// The new-grant form's choices.
	Connectors []connectorChoice `json:"connectors"`
	For        []option          `json:"for_options"`
	Length     []option          `json:"length_options"`
	Horizon    []option          `json:"horizon_options"`
}

type agentItem struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	FirstSeen time.Time `json:"first_seen,omitzero"`
	LastSeen  time.Time `json:"last_seen,omitzero"`
	// Grants are its windows not yet over, Ended those closed or run out
	// in the last 30 days.
	Grants   []grantView `json:"grants"`
	Routines []grantView `json:"routines"`
	Ended    []grantView `json:"ended"`
}

type connectorChoice struct {
	Name        string `json:"name"`
	ForwardOnly bool   `json:"forward_only,omitempty"`
}

func (s *Server) agentsPage(w http.ResponseWriter, r *http.Request, sess session) {
	d, err := s.agentsData(r.Context())
	if err != nil {
		s.cfg.Log.Error("load agents", "err", err)
		s.errorPage(w, http.StatusInternalServerError, "Couldn't load the agents.")
		return
	}
	s.owner(w, r, sess, "agents", "Agents", "agents", d)
}

func (s *Server) agentsJSON(w http.ResponseWriter, r *http.Request, _ session) {
	d, err := s.agentsData(r.Context())
	if err != nil {
		connector.WriteError(w, err)
		return
	}
	connector.WriteJSON(w, http.StatusOK, d)
}

func (s *Server) agentsData(ctx context.Context) (agentsData, error) {
	now := s.now()
	d := agentsData{
		GatewayURL: s.cfg.GatewayURL,
		CanSign:    len(s.keys.Trusted()) > 0,
		For:        spanOptions(grantChoices, 0, time.Hour, span),
		Length:     spanOptions(forChoices, 0, time.Hour, span),
		Horizon:    spanOptions(horizonChoices, 0, 7*24*time.Hour, horizonText),
	}
	d.ReleaseURL, d.SkillURL = sources()
	fo := s.forwardOnlyAll(ctx)
	for _, name := range slices.Sorted(maps.Keys(s.conns)) {
		d.Connectors = append(d.Connectors, connectorChoice{Name: name, ForwardOnly: fo[name]})
	}
	agents, err := s.db.agents(ctx)
	if err != nil {
		return d, err
	}
	grants, err := s.db.grantsSince(ctx, now.Add(-stoppedKept))
	if err != nil {
		return d, err
	}
	d.Agents = []agentItem{}
	for _, a := range agents {
		it := agentItem{
			ID: a.ID, Name: a.Name, CreatedAt: a.CreatedAt, FirstSeen: a.FirstSeen, LastSeen: a.LastSeen,
			Grants: []grantView{}, Routines: []grantView{}, Ended: []grantView{},
		}
		for _, g := range grants {
			if g.Agent != a.ID || g.ReplacedBy != "" {
				continue
			}
			v := grantViewOf(g, a.Name, fo[g.Connector], now)
			switch {
			case g.RevokedAt.IsZero() && g.Schedule != nil:
				it.Routines = append(it.Routines, v)
			case g.RevokedAt.IsZero() && (v.Open || !v.Next.IsZero()):
				it.Grants = append(it.Grants, v)
			default:
				it.Ended = append(it.Ended, v)
			}
		}
		slices.Reverse(it.Ended)
		d.Agents = append(d.Agents, it)
	}
	return d, nil
}
