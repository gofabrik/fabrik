import htmx from "htmx.org";
import { highlightCurrent } from "./nav.js";

// htmx's injected indicator style violates the CSP, so style.css defines it.
htmx.config.includeIndicatorStyles = false;
window.htmx = htmx;
highlightCurrent(document);
