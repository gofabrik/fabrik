export function highlightCurrent(doc) {
	const here = doc.location.pathname;
	for (const link of doc.querySelectorAll("nav a")) {
		if (link.getAttribute("href") === here) {
			link.setAttribute("aria-current", "page");
		}
	}
}
