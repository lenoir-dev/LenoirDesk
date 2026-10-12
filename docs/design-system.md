# LenoirDesk design system

LenoirDesk is the visible product name. The Go module, database schema, API
paths, environment variables, session-cookie names and deployment identifiers
retain their `relaydesk` names for compatibility. Presentation changes do not
alter integration contracts or business logic.

## Identity and visual rules

The LD monogram in `internal/api/assets/lenoirdesk-mark.svg` is the shared app
mark, sidebar mark and favicon. It is a self-hosted vector asset with no
external font or image dependency. The wordmark pairs a strong “Lenoir” with a
lighter “Desk”. Avoid decorative mascots, neon colors and chat-app navigation.

`internal/api/assets/app.css` is the single source for the visual system. Its
tokens include canvas, ink, muted text, wine accent, surface, border, radius,
shadow and status colors for light and dark themes. Light keeps the warm
`#f8f5f5` canvas and `#772844` action color; dark uses graphite `#171117`,
blackberry surfaces and a brighter wine accent. Success, warning, danger and
informational states have separate foreground/background pairs. The default
theme is dark; Light and System are explicit alternatives.

Glass is restrained: standard cards, long forms and tables stay opaque, while
navigation, workspace switching, popovers, the entry panel and selected
dashboard panels use translucency and blur. A solid theme-matched fallback
applies where backdrop blur is unsupported. System fonts avoid a network
request. All
interactive controls share focus treatment, short transitions and consistent
spacing. `prefers-reduced-motion` removes animation and smooth scrolling.
Tables keep horizontal scrolling on narrow screens; status colors are paired
with visible text rather than acting as the sole signal.

## Shell and localization

The pre-auth entry, workspace selection and authenticated shell now share the
mark, palette, cards, controls and language switcher. The authenticated shell
provides a workspace switch, section navigation, current-page context and an
account menu with sign-out. On narrow screens the navigation becomes a
horizontal list and sign-out remains available in the account menu.

English and Russian are the only reviewed languages. A `lang=en|ru` query
chooses the language for the current local route; a host-only, HttpOnly,
SameSite=Lax cookie remembers it through login and navigation. Unknown values
fall back to the saved language or English. The choice changes presentation
only and never affects authorization, tenant context or business data. Theme
selection follows the same server-rendered model with `theme=light|dark|system`
and a host-only, HttpOnly, SameSite=Lax cookie. System uses
`prefers-color-scheme`; the root theme attribute is rendered before CSS loads
so the initial paint uses the chosen palette.

The dictionary covers the entry screen, workspace selection, global shell,
Overview, Connections and shared notices. Automation, Runs, Reconciliation
and Run Details currently retain English-only page content. The global shell
and styling apply to them.

Overview shows real persisted tenant-scoped counts. Its one chart aggregates
the last 14 UTC calendar days with an indexed, bounded query and fills absent
days with zeroes. The chart appears only when at least one run exists in that
window; a semantic table provides the exact per-day values for assistive
technology. Empty workspaces lead with setup, while active workspaces show
operations first and collapse the setup checklist.

Connections groups provider rows under Stripe, HubSpot and Slack cards. A
provider is marked verified only when active, health-verified and not blocked
by its OAuth authorization state. Exact account identity, Test Mode context,
verification timestamps and safe remediation remain visible. Owner-only
setup and reconnect actions retain server-side authorization and CSRF checks.
Long setup details are progressively disclosed.

## Security and remaining interface work

Styles, icon and HTMX are served from a fixed embedded allowlist. The browser
policy uses `style-src 'self'` and `img-src 'self'`; no CDN, inline style or
new client-side state library is required. Existing OIDC, CSRF, RBAC and
tenant-scoped routes are unchanged.

The local Dex password page uses its officially supported frontend theming
path to show the LD mark and a matching dark wine form. Dex continues to own
credentials and the OIDC flow. Automation, Runs, Reconciliation and Run Details
still require full localization while preserving the underlying reliability and
safety contracts.
