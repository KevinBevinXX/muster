// An in-app link to an agent's page names its session: /agents/{name}?session=<id>
// (ui.AgentSessionHref), so a tap skips the bare URL's 303. An agent with no
// session yet is still linked bare. Specs that find a card BY its link use
// these, so they match either form and nothing else (not /agents/{name}-other,
// not /agents/{name}/terminal).
export const agentLink = (name: string): string => `a[href="/agents/${name}"], a[href^="/agents/${name}?session="]`;

// The path of an href, without its query: what "which agent" a link names.
export const hrefPath = (h: string | null): string => (h || '').split('?')[0];

// The canonical session URL of an agent's page.
export const agentPageRe = (name: string): RegExp => new RegExp(`^/agents/${name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\?session=\\d+$`);
