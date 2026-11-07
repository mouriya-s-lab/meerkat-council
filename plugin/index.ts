/**
 * pm-council omp plugin.
 *
 * The council program starts every seat session with:
 * - PM_KIND: "draft" | "vote" | "revise" | "arbitrate"
 * - PM_GOAL_TOOLS: comma-separated tools of this kind whose successful call completes the session
 * - PM_CONTINUE: the continue text injected while no goal tool has succeeded
 * - PM_META: JSON recorded as the `pm-meta` custom entry of the session
 * - PM_CONTEXT: path of a JSON file with the seat, its rights and proxy-read mapping, the PM
 *   repository, the repositories by category, the human issues, the draft, the escalation, the
 *   row categories (tally.yaml) and the rows an arbitrate session rules on
 * - PM_GUARDIANS_URL: base URL of pm-guardians, which serves proxy reads
 * - GH_TOKEN: read access to the PM repository
 *
 * The plugin reads GitHub but never writes it: a goal tool only validates its arguments; the
 * program takes them from the RPC event stream and writes the markers. An invalid call throws,
 * so the tool result is an error, the goal does not count and the session continues.
 */
import { readFileSync } from "node:fs";
import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { checkDeliver, emptyDeliverFields, type DeliverCheck } from "./validate.ts";

type Content = "code" | "design" | "omp";

interface Row {
	id: string;
}

interface Context {
	ref: string;
	seat: string;
	rights: string[];
	pmRepo: string;
	repos: Record<Content, string[]>;
	proxy: { seat: string; default: string; others: string[]; content: Content[]; note: string };
	humanIssues: { ref: string; decided: boolean }[];
	draft: { version: number; rows: Row[] } | null;
	escalation: { issue: string; ask: number } | null;
	categories: string[];
	disputed: string[];
}

function requiredEnv(name: string): string {
	const value = process.env[name];
	if (value === undefined || value === "") throw new Error(`pm-council plugin: ${name} is not set`);
	return value;
}

const sameRef = (a: string, b: string) => a.trim().toLowerCase() === b.trim().toLowerCase();

function parseRef(ref: string): { repo: string; number: number } | null {
	const m = /^([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)#([1-9][0-9]*)$/.exec(ref.trim());
	return m ? { repo: m[1], number: Number(m[2]) } : null;
}

async function github(path: string, signal: AbortSignal | undefined): Promise<{ body: unknown; next: string | null }> {
	const token = requiredEnv("GH_TOKEN");
	const res = await fetch(path.startsWith("https://") ? path : `https://api.github.com/${path}`, {
		headers: { Authorization: `Bearer ${token}`, Accept: "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28" },
		signal,
	});
	if (!res.ok) throw new Error(`GitHub GET ${path}: ${res.status} ${await res.text()}`);
	const next = /<([^>]+)>;\s*rel="next"/.exec(res.headers.get("link") ?? "");
	return { body: await res.json(), next: next ? next[1] : null };
}

interface IssueBody {
	title: string;
	state: string;
	body: string | null;
	user: { login: string } | null;
}

interface CommentBody {
	id: number;
	body: string | null;
	created_at: string;
	user: { login: string } | null;
}

async function readIssue(repo: string, number: number, signal: AbortSignal | undefined): Promise<string> {
	const issue = (await github(`repos/${repo}/issues/${number}`, signal)).body as IssueBody;
	let text = `# ${issue.title}\n\n${repo}#${number} · ${issue.state} · by ${issue.user?.login ?? "?"}\n\n${issue.body ?? ""}`;
	let page: string | null = `repos/${repo}/issues/${number}/comments?per_page=100`;
	while (page) {
		const { body, next } = await github(page, signal);
		for (const c of body as CommentBody[]) {
			text += `\n\n---\n## comment ${c.id} by ${c.user?.login ?? "?"} at ${c.created_at}\n\n${c.body ?? ""}`;
		}
		page = next;
	}
	return text;
}

export default function pmCouncil(pi: ExtensionAPI): void {
	const kind = requiredEnv("PM_KIND");
	if (kind !== "draft" && kind !== "vote" && kind !== "revise" && kind !== "arbitrate") {
		throw new Error(`pm-council plugin: PM_KIND ${kind} is not draft, vote, revise or arbitrate`);
	}
	const goalTools = new Set(requiredEnv("PM_GOAL_TOOLS").split(","));
	const continueText = requiredEnv("PM_CONTINUE");
	const meta: unknown = JSON.parse(requiredEnv("PM_META"));
	const guardiansURL = requiredEnv("PM_GUARDIANS_URL").replace(/\/+$/, "");
	const ctx = JSON.parse(readFileSync(requiredEnv("PM_CONTEXT"), "utf8")) as Context;
	const z = pi.zod;
	let goalReached = false;

	const recorded = (name: string) => {
		if (goalTools.has(name)) goalReached = true;
		return { content: [{ type: "text" as const, text: "recorded" }] };
	};

	const proxyModels = [ctx.proxy.default, ...ctx.proxy.others];
	const proxyRepos = ctx.proxy.content.flatMap(c => ctx.repos[c]);
	const humanRefs = ctx.humanIssues.map(h => h.ref);

	pi.on("session_start", () => {
		pi.appendEntry("pm-meta", meta);
	});

	pi.registerTool({
		name: "pm_read_issue",
		label: "Read PM issue",
		description: `Read an issue of the PM repository ${ctx.pmRepo}: title, body and every comment.`,
		parameters: z.object({
			ref: z.string().describe(`Issue reference ${ctx.pmRepo}#<number>`),
		}),
		loadMode: "essential",
		approval: "read",
		async execute(_id, params, signal) {
			const ref = parseRef(params.ref);
			if (!ref || !sameRef(ref.repo, ctx.pmRepo)) {
				throw new Error(`ref must be ${ctx.pmRepo}#<number>; got ${params.ref}`);
			}
			return { content: [{ type: "text" as const, text: await readIssue(ref.repo, ref.number, signal) }] };
		},
	});

	pi.registerTool({
		name: "pm_proxy_read",
		label: "Proxy read",
		description:
			`Ask the intent guardians to read a code, design or omp repository for you and return facts with sources. ` +
			`Repositories you may have read: ${proxyRepos.join(", ") || "none"}. Models: ${proxyModels.join(", ")} (default ${ctx.proxy.default}).`,
		parameters: z.object({
			repo: z.string().describe(`Repository owner/name, one of: ${proxyRepos.join(", ") || "none"}`),
			question: z.string().describe("What to find out in that repository"),
			model: z.string().optional().describe(`Proxy model; default ${ctx.proxy.default}`),
		}),
		loadMode: "essential",
		approval: "read",
		async execute(_id, params, signal) {
			const model = params.model ?? ctx.proxy.default;
			if (!proxyModels.includes(model)) {
				throw new Error(
					`model ${model} is not in the proxy-read mapping of seat ${ctx.seat} (proxy-readers.yaml: default ${ctx.proxy.default}, others [${ctx.proxy.others.join(", ")}])`,
				);
			}
			if (!proxyRepos.some(r => sameRef(r, params.repo))) {
				throw new Error(
					`repo ${params.repo} is not readable by seat ${ctx.seat}: proxy-readers.yaml content [${ctx.proxy.content.join(", ")}] covers [${proxyRepos.join(", ")}]`,
				);
			}
			// A proxy read is a whole guardian session and may outlast Bun's default fetch timeout;
			// the call ends with the guardians' answer or with the tool's abort signal.
			const init: RequestInit & { timeout: false } = {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({ seat: ctx.seat, model, repo: params.repo, question: params.question, agenda: ctx.ref }),
				signal,
				timeout: false,
			};
			const res = await fetch(`${guardiansURL}/v1/proxy-reads`, init);
			const text = await res.text();
			if (res.status !== 200) throw new Error(`pm-guardians ${res.status}: ${text}`);
			const body = JSON.parse(text) as { session: string; facts: string };
			return { content: [{ type: "text" as const, text: body.facts }] };
		},
	});

	const categories = ctx.categories.join(", ");
	if (kind === "draft" || kind === "revise") {
		const deliver = z.object({ repo: z.string(), title: z.string(), body: z.string() });
		// One wording for the rule, shared by the schema description (which omp's validation error
		// quotes as "withdrawn must be <description>") and the execute-time check.
		const draftWithdrawn = "null: only a revise session may withdraw the agenda";
		const row = z.object({
			id: z.string().describe("Row id <agenda>-<two-digit sequence>, e.g. D3-01"),
			what: z.string(),
			where: z.string(),
			how: z.string(),
			tradeoff: z.string(),
			category: z.string().describe(`One of: ${categories}`),
			missingDesign: z.boolean(),
			deliver: deliver
				.nullable()
				.optional()
				.describe(`Implementation issue to create in one of ${ctx.repos.code.join(", ") || "no repository"}; omit this field (do not pass a key at all) when the row has no implementation issue`),
		});
		pi.registerTool({
			name: "pm_draft",
			label: "Submit draft",
			description: "Submit this panel's decision table; a successful call ends your task. The program assigns the version.",
			parameters: z.object({
				rows: z.array(row),
				supersedes: z.array(z.string()).describe("Old agenda issue refs a human issue changes; [] for agenda issues"),
				settle: z
					.object({ kind: z.enum(["reply", "replace"]), text: z.string() })
					.nullable()
					.optional()
					.describe("How the escalated ask is settled; null when the issue was not raised from an ask"),
				withdrawn: z
					.string()
					.nullable()
					.optional()
					.describe(kind === "revise" ? "Reason the agenda is withdrawn, or null" : draftWithdrawn),
			}),
			loadMode: "essential",
			approval: "read",
			async execute(_id, params) {
				// An omitted settle or withdrawn is null, as an omitted deliver is no delivery.
				const settle = params.settle ?? null;
				const withdrawn = params.withdrawn ?? null;
				if (kind === "draft" && withdrawn !== null) throw new Error(`withdrawn must be ${draftWithdrawn}`);
				if (withdrawn !== null && withdrawn.trim() === "") throw new Error("withdrawn is an empty reason; give the reason or null");
				if (params.rows.length === 0 && withdrawn === null) throw new Error("rows is empty and withdrawn is null");
				const ids = new Set<string>();
				const code: DeliverCheck = { repos: ctx.repos.code };
				for (const [i, r] of params.rows.entries()) {
					if (r.id.trim() === "") throw new Error(`rows[${i}].id is empty`);
					if (ids.has(r.id)) throw new Error(`rows[${i}].id ${r.id} is used twice`);
					ids.add(r.id);
					if (!ctx.categories.includes(r.category)) throw new Error(`rows[${i}] (${r.id}): category ${r.category} is not one of: ${categories}`);
					if (r.deliver !== undefined && r.deliver !== null) {
						const empties = emptyDeliverFields(r.deliver.repo, r.deliver.title, r.deliver.body);
						if (empties.length > 0) throw new Error(`rows[${i}] (${r.id}): deliver.${empties.join(", deliver.")} must be non-blank; omit deliver entirely (no key) for a row with no implementation issue`);
						const notAllowed = checkDeliver(code, r.id, r.deliver);
						if (notAllowed !== null) throw new Error(notAllowed);
					}
				}
				if (ctx.escalation && settle === null) {
					throw new Error(`settle required: ${ctx.escalation.issue} ask ${ctx.escalation.ask}`);
				}
				return recorded("pm_draft");
			},
		});
	} else if (kind === "vote") {
		const voteRow = z.object({
			row: z.string().describe("Row id of the draft"),
			choice: z.enum(["approve", "oppose", "veto"]),
			counterexample: z.string().nullable().describe("Reproducible counterexample; required for oppose"),
			citation: z.string().nullable().describe("Human issue ref the veto cites; required for veto"),
			classification: z.string().nullable().describe("Your view of the row's category, or null"),
		});
		pi.registerTool({
			name: "pm_vote",
			label: "Vote",
			description: "Cast this seat's vote on every row of the current draft; a successful call ends your task.",
			parameters: z.object({ rows: z.array(voteRow) }),
			loadMode: "essential",
			approval: "read",
			async execute(_id, params) {
				const draft = ctx.draft;
				if (!draft) throw new Error("no draft to vote on");
				const want = draft.rows.map(r => r.id);
				const seen = new Set<string>();
				for (const [i, r] of params.rows.entries()) {
					if (!want.includes(r.row)) throw new Error(`rows[${i}].row ${r.row} is not a row of draft v${draft.version}: [${want.join(", ")}]`);
					if (seen.has(r.row)) throw new Error(`rows[${i}].row ${r.row} is voted twice`);
					seen.add(r.row);
					if (r.choice === "oppose" && !r.counterexample?.trim()) {
						throw new Error(`rows[${i}] (${r.row}): oppose needs a non-empty counterexample`);
					}
					if (r.choice === "veto") {
						if (!ctx.rights.includes("veto")) throw new Error(`rows[${i}] (${r.row}): seat ${ctx.seat} has no veto right`);
						if (!r.citation || !humanRefs.some(h => sameRef(h, r.citation ?? ""))) {
							throw new Error(`rows[${i}] (${r.row}): veto must cite a human issue: [${humanRefs.join(", ")}]`);
						}
					}
				}
				const missing = want.filter(id => !seen.has(id));
				if (missing.length > 0) throw new Error(`rows missing for draft v${draft.version}: [${missing.join(", ")}]`);
				return recorded("pm_vote");
			},
		});
	} else {
		const disputed = ctx.disputed;
		pi.registerTool({
			name: "pm_ruling",
			label: "Rule categories",
			description: `Rule the category of every disputed row (${disputed.join(", ")}); a successful call ends your task.`,
			parameters: z.object({
				rows: z.array(z.object({ row: z.string().describe(`Disputed row id: ${disputed.join(", ")}`), category: z.string().describe(`One of: ${categories}`) })),
			}),
			loadMode: "essential",
			approval: "read",
			async execute(_id, params) {
				const seen = new Set<string>();
				for (const [i, r] of params.rows.entries()) {
					if (!disputed.includes(r.row)) throw new Error(`rows[${i}].row ${r.row} is not disputed: [${disputed.join(", ")}]`);
					if (seen.has(r.row)) throw new Error(`rows[${i}].row ${r.row} is ruled twice`);
					seen.add(r.row);
					if (!ctx.categories.includes(r.category)) throw new Error(`rows[${i}] (${r.row}): category ${r.category} is not one of: ${categories}`);
				}
				const missing = disputed.filter(id => !seen.has(id));
				if (missing.length > 0) throw new Error(`disputed rows not ruled: [${missing.join(", ")}]`);
				return recorded("pm_ruling");
			},
		});
	}

	pi.on("session_stop", () => {
		if (goalReached) return undefined;
		return { continue: true, additionalContext: continueText };
	});
}
