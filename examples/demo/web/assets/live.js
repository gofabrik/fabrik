const ticker = document.getElementById("ticker");
if (ticker) {
	const source = new EventSource("/live/events");
	source.onmessage = (event) => {
		const li = document.createElement("li");
		li.textContent = event.data;
		ticker.append(li);
		while (ticker.children.length > 20) {
			ticker.firstElementChild.remove();
		}
	};
}
