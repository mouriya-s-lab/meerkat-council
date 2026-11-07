/**
 * Validation shared by the pm_draft tool and the publish path's pre-CreateIssue check duty: a
 * present `deliver` must carry a non-blank repo, title and body; undefined/null mean no delivery.
 */

/** A deliver checker bound to the code repositories of one context. */
export interface DeliverCheck {
	repos: string[];
}

const sameRef = (a: string, b: string) => a.trim().toLowerCase() === b.trim().toLowerCase();

// The repo/title/body fields whose blank value makes the deliver invalid, in field order.
export function emptyDeliverFields(repo: string, title: string, body: string): string[] {
	const empties: string[] = [];
	if (repo.trim() === "") empties.push("repo");
	if (title.trim() === "") empties.push("title");
	if (body.trim() === "") empties.push("body");
	return empties;
}

export function checkDeliver(check: DeliverCheck, id: string, deliver: { repo: string; title: string; body: string } | null | undefined): string | null {
	if (deliver === null || deliver === undefined) return null;
	const empties = emptyDeliverFields(deliver.repo, deliver.title, deliver.body);
	if (empties.length > 0) {
		return `rows (${id}): deliver.${empties.join(", deliver.")} is empty`;
	}
	if (!check.repos.some(c => sameRef(c, deliver.repo))) {
		return `rows (${id}): deliver.repo ${deliver.repo} is not a code repository: [${check.repos.join(", ")}]`;
	}
	return null;
}

export const isCodeRepo = (check: DeliverCheck, repo: string): boolean => check.repos.some(c => sameRef(c, repo));
