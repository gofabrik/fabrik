import htmx from "htmx.org";
import { highlightCurrent } from "./nav.js";

window.htmx = htmx;
highlightCurrent(document);
