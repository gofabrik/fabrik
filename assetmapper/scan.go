package assetmapper

import (
	"strings"
	"unicode/utf8"
)

// scanJSImportRefs is a small lexical scanner for module specifier string
// literals. It deliberately does not try to parse JavaScript expressions, but
// it does skip comments, strings, templates, and regular-expression literals
// so words that merely look like imports cannot become build dependencies.
func scanJSImportRefs(content []byte) []scannedRef {
	var refs []scannedRef
	scanJSCode(content, 0, false, &refs)
	return refs
}

func scanJSCode(content []byte, start int, stopAtBrace bool, refs *[]scannedRef) int {
	canStartRegex := true
	braceDepth := 0
	previousDot := false
	controlHeader := false
	var statementParens []bool
	i := start
	if start == 0 {
		hashbang := 0
		if len(content) >= 3 && string(content[:3]) == "\ufeff" {
			hashbang = 3
		}
		if hashbang+1 < len(content) && content[hashbang] == '#' && content[hashbang+1] == '!' {
			i = skipLineComment(content, hashbang+2)
		}
	}
	for i < len(content) {
		if next := skipJSSpaces(content, i); next != i {
			i = next
			continue
		}
		switch {
		case i+1 < len(content) && content[i] == '/' && content[i+1] == '/':
			i = skipLineComment(content, i+2)
		case i+1 < len(content) && content[i] == '/' && content[i+1] == '*':
			i = skipBlockComment(content, i+2)
		case content[i] == '\'' || content[i] == '"':
			i = skipQuoted(content, i, content[i])
			canStartRegex = false
			previousDot = false
			controlHeader = false
		case content[i] == '`':
			i = scanJSTemplate(content, i+1, refs)
			canStartRegex = false
			previousDot = false
			controlHeader = false
		case content[i] == '/' && canStartRegex:
			if end, ok := skipJSRegex(content, i+1); ok {
				i = end
				canStartRegex = false
				previousDot = false
				controlHeader = false
			} else {
				i++
				canStartRegex = true
				previousDot = false
				controlHeader = false
			}
		case isJSIdentifierStart(content[i]):
			end := scanJSIdentifier(content, i)
			word := string(content[i:end])
			if !previousDot {
				var candidate scannedRef
				var ok bool
				switch word {
				case "import":
					candidate, ok = scanJSImportDeclaration(content, end)
				case "export":
					candidate, ok = scanJSExportDeclaration(content, end)
				}
				if ok {
					*refs = append(*refs, candidate)
				}
			}
			canStartRegex = jsKeywordAllowsExpression(word)
			previousDot = false
			controlHeader = isJSControlHeader(word) || controlHeader && word == "await"
			i = end
		case isASCIIDigit(content[i]):
			i = skipJSNumber(content, i)
			canStartRegex = false
			previousDot = false
			controlHeader = false
		default:
			switch content[i] {
			case '{':
				braceDepth++
				canStartRegex = true
				previousDot = false
				controlHeader = false
			case '}':
				if stopAtBrace && braceDepth == 0 {
					return i + 1
				}
				if braceDepth > 0 {
					braceDepth--
				}
				canStartRegex = true
				previousDot = false
				controlHeader = false
			case '(':
				statementParens = append(statementParens, controlHeader)
				canStartRegex = true
				previousDot = false
				controlHeader = false
			case ')':
				canStartRegex = false
				if last := len(statementParens) - 1; last >= 0 {
					canStartRegex = statementParens[last]
					statementParens = statementParens[:last]
				}
				previousDot = false
				controlHeader = false
			case ']':
				canStartRegex = false
				previousDot = false
				controlHeader = false
			case '.', '#':
				canStartRegex = false
				previousDot = true
				controlHeader = false
			case '+', '-':
				double := i+1 < len(content) && content[i+1] == content[i]
				canStartRegex = !double
				previousDot = false
				controlHeader = false
				if double {
					i++
				}
			default:
				canStartRegex = true
				previousDot = false
				controlHeader = false
			}
			i++
		}
	}
	return len(content)
}

func scanJSTemplate(content []byte, start int, refs *[]scannedRef) int {
	for i := start; i < len(content); {
		switch {
		case content[i] == '\\':
			i += 2
		case content[i] == '`':
			return i + 1
		case i+1 < len(content) && content[i] == '$' && content[i+1] == '{':
			i = scanJSCode(content, i+2, true, refs)
		default:
			i++
		}
	}
	return len(content)
}

type jsLexemeKind uint8

const (
	jsLexemeOther jsLexemeKind = iota
	jsLexemeIdentifier
	jsLexemeString
	jsLexemeTemplate
	jsLexemePunct
)

type jsLexeme struct {
	kind       jsLexemeKind
	start, end int
	value      string
}

func nextJSLexeme(content []byte, start int) (jsLexeme, int) {
	i := start
	for i < len(content) {
		if next := skipJSSpaces(content, i); next != i {
			i = next
			continue
		}
		switch {
		case i+1 < len(content) && content[i] == '/' && content[i+1] == '/':
			i = skipLineComment(content, i+2)
		case i+1 < len(content) && content[i] == '/' && content[i+1] == '*':
			i = skipBlockComment(content, i+2)
		default:
			goto found
		}
	}
found:
	if i >= len(content) {
		return jsLexeme{}, i
	}
	if content[i] == '\'' || content[i] == '"' {
		end := skipQuoted(content, i, content[i])
		innerEnd := end
		if end > i && end <= len(content) && content[end-1] == content[i] {
			innerEnd--
		}
		return jsLexeme{
			kind:  jsLexemeString,
			start: i + 1,
			end:   innerEnd,
			value: decodeJSSpecifier(content[i+1 : innerEnd]),
		}, end
	}
	if content[i] == '`' {
		for end := i + 1; end < len(content); end++ {
			if content[end] == '\\' {
				end++
				continue
			}
			if end+1 < len(content) && content[end] == '$' && content[end+1] == '{' {
				return jsLexeme{kind: jsLexemeOther, start: i, end: end + 2}, end + 2
			}
			if content[end] == '`' {
				return jsLexeme{
					kind:  jsLexemeTemplate,
					start: i + 1,
					end:   end,
					value: decodeJSSpecifier(content[i+1 : end]),
				}, end + 1
			}
		}
		return jsLexeme{kind: jsLexemeOther, start: i, end: len(content)}, len(content)
	}
	if isJSIdentifierStart(content[i]) {
		end := scanJSIdentifier(content, i)
		return jsLexeme{
			kind:  jsLexemeIdentifier,
			start: i,
			end:   end,
			value: string(content[i:end]),
		}, end
	}
	return jsLexeme{
		kind:  jsLexemePunct,
		start: i,
		end:   i + 1,
		value: string(content[i]),
	}, i + 1
}

func scanJSImportDeclaration(content []byte, start int) (scannedRef, bool) {
	token, next := nextJSLexeme(content, start)
	if token.kind == jsLexemeString {
		return scannedRef{
			start: token.start,
			end:   token.end,
			kind:  referenceJSImport,
			value: token.value,
		}, true
	}
	if token.kind == jsLexemePunct && token.value == "." {
		return scannedRef{}, false // import.meta
	}
	if token.kind == jsLexemePunct && token.value == "(" {
		token, _ = nextJSLexeme(content, next)
		if token.kind == jsLexemeString || token.kind == jsLexemeTemplate {
			return scannedRef{
				start: token.start,
				end:   token.end,
				kind:  referenceJSImport,
				value: token.value,
			}, true
		}
		return scannedRef{}, false
	}
	return scanJSFromClause(content, next)
}

func scanJSExportDeclaration(content []byte, start int) (scannedRef, bool) {
	token, next := nextJSLexeme(content, start)
	if token.kind != jsLexemePunct || token.value != "*" && token.value != "{" {
		return scannedRef{}, false
	}
	return scanJSFromClause(content, next)
}

func scanJSFromClause(content []byte, start int) (scannedRef, bool) {
	for pos := start; pos < len(content); {
		token, next := nextJSLexeme(content, pos)
		if token.end == 0 || (token.kind == jsLexemePunct && token.value == ";") {
			return scannedRef{}, false
		}
		if token.kind == jsLexemeIdentifier && token.value == "from" {
			spec, _ := nextJSLexeme(content, next)
			if spec.kind == jsLexemeString {
				return scannedRef{
					start: spec.start,
					end:   spec.end,
					kind:  referenceJSImport,
					value: spec.value,
				}, true
			}
		}
		pos = next
	}
	return scannedRef{}, false
}

func scanCSSRefs(content []byte) []scannedRef {
	var refs []scannedRef
	for i := 0; i < len(content); {
		switch {
		case isCSSSpace(content[i]):
			i++
		case i+1 < len(content) && content[i] == '/' && content[i+1] == '*':
			i = skipBlockComment(content, i+2)
		case content[i] == '\'' || content[i] == '"':
			i = skipQuoted(content, i, content[i])
		case content[i] == '@':
			nameStart := i + 1
			nameEnd, name := scanCSSIdentifier(content, nameStart)
			if nameEnd > nameStart && strings.EqualFold(name, "import") {
				if candidate, next, ok := scanCSSImportTarget(content, nameEnd); ok {
					refs = append(refs, candidate)
					i = next
					continue
				}
			}
			i = max(nameEnd, i+1)
		case isCSSIdentifierStart(content[i]):
			nameEnd, name := scanCSSIdentifier(content, i)
			if strings.EqualFold(name, "url") &&
				nameEnd < len(content) && content[nameEnd] == '(' {
				if candidate, next, ok := scanCSSURLTarget(content, nameEnd, referenceCSSURL); ok {
					refs = append(refs, candidate)
					i = next
					continue
				}
			}
			i = nameEnd
		default:
			i++
		}
	}
	return refs
}

func scanCSSImportTarget(content []byte, start int) (scannedRef, int, bool) {
	i := skipCSSWhitespaceAndComments(content, start)
	if i >= len(content) {
		return scannedRef{}, i, false
	}
	if content[i] == '\'' || content[i] == '"' {
		end := skipQuoted(content, i, content[i])
		innerEnd := end
		if end > i && end <= len(content) && content[end-1] == content[i] {
			innerEnd--
		}
		return scannedRef{
			start: i + 1,
			end:   innerEnd,
			kind:  referenceCSSImport,
			value: decodeCSSSpecifier(content[i+1 : innerEnd]),
		}, end, true
	}
	nameEnd, name := scanCSSIdentifier(content, i)
	if nameEnd > i && strings.EqualFold(name, "url") &&
		nameEnd < len(content) && content[nameEnd] == '(' {
		return scanCSSURLTarget(content, nameEnd, referenceCSSImport)
	}
	return scannedRef{}, i, false
}

func scanCSSURLTarget(content []byte, open int, kind referenceKind) (scannedRef, int, bool) {
	i := skipCSSWhitespaceAndComments(content, open+1)
	if i >= len(content) {
		return scannedRef{}, i, false
	}
	if content[i] == '\'' || content[i] == '"' {
		end := skipQuoted(content, i, content[i])
		innerEnd := end
		if end > i && end <= len(content) && content[end-1] == content[i] {
			innerEnd--
		}
		return scannedRef{
			start: i + 1,
			end:   innerEnd,
			kind:  kind,
			value: decodeCSSSpecifier(content[i+1 : innerEnd]),
		}, end, true
	}
	start := i
	for i < len(content) && content[i] != ')' && !isCSSSpace(content[i]) {
		if i+1 < len(content) && content[i] == '/' && content[i+1] == '*' {
			break
		}
		if content[i] == '\\' {
			if _, next, ok := decodeCSSEscape(content, i); ok {
				i = next
				continue
			}
		}
		i++
	}
	if i == start {
		return scannedRef{}, i, false
	}
	return scannedRef{
		start: start,
		end:   i,
		kind:  kind,
		value: decodeCSSSpecifier(content[start:i]),
	}, i, true
}

func skipCSSWhitespaceAndComments(content []byte, start int) int {
	i := start
	for i < len(content) {
		switch {
		case isCSSSpace(content[i]):
			i++
		case i+1 < len(content) && content[i] == '/' && content[i+1] == '*':
			i = skipBlockComment(content, i+2)
		default:
			return i
		}
	}
	return i
}

func skipLineComment(content []byte, start int) int {
	for i := start; i < len(content); {
		if content[i] == '\n' || content[i] == '\r' {
			return i
		}
		if isJSUnicodeLineTerminator(content[i:]) {
			return i
		}
		_, width := utf8.DecodeRune(content[i:])
		i += width
	}
	return len(content)
}

func skipBlockComment(content []byte, start int) int {
	for i := start; i+1 < len(content); i++ {
		if content[i] == '*' && content[i+1] == '/' {
			return i + 2
		}
	}
	return len(content)
}

func skipQuoted(content []byte, start int, quote byte) int {
	for i := start + 1; i < len(content); i++ {
		if content[i] == '\\' {
			i++
			continue
		}
		if content[i] == quote {
			return i + 1
		}
		if (quote == '\'' || quote == '"') && (content[i] == '\n' || content[i] == '\r') {
			return i
		}
	}
	return len(content)
}

func skipJSRegex(content []byte, start int) (int, bool) {
	inClass := false
	for i := start; i < len(content); i++ {
		if isJSUnicodeLineTerminator(content[i:]) {
			return i, false
		}
		switch content[i] {
		case '\\':
			i++
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				i++
				for i < len(content) && isJSIdentifierPart(content[i]) {
					i++
				}
				return i, true
			}
		case '\n', '\r':
			return i, false
		}
	}
	return len(content), false
}

func skipJSNumber(content []byte, start int) int {
	i := start + 1
	for i < len(content) {
		c := content[i]
		if !isASCIIDigit(c) && !isASCIIAlpha(c) && c != '.' && c != '_' {
			break
		}
		i++
	}
	return i
}

func scanJSIdentifier(content []byte, start int) int {
	i := start
	for i < len(content) {
		if content[i] < utf8.RuneSelf {
			if !isJSIdentifierPart(content[i]) {
				break
			}
			i++
			continue
		}
		value, width := utf8.DecodeRune(content[i:])
		if isJSUnicodeSpace(value) {
			break
		}
		i += width
	}
	return i
}

func scanCSSIdentifier(content []byte, start int) (int, string) {
	var decoded strings.Builder
	i := start
	for i < len(content) {
		if isCSSIdentifierPart(content[i]) && content[i] != '\\' {
			decoded.WriteByte(content[i])
			i++
			continue
		}
		if content[i] == '\\' {
			value, next, ok := decodeCSSEscape(content, i)
			if !ok {
				break
			}
			decoded.WriteRune(value)
			i = next
			continue
		}
		break
	}
	return i, decoded.String()
}

func decodeJSSpecifier(raw []byte) string {
	var decoded strings.Builder
	for i := 0; i < len(raw); {
		if raw[i] != '\\' {
			decoded.WriteByte(raw[i])
			i++
			continue
		}
		i++
		if i >= len(raw) {
			decoded.WriteByte('\\')
			break
		}
		switch raw[i] {
		case '\n':
			i++
		case '\r':
			i++
			if i < len(raw) && raw[i] == '\n' {
				i++
			}
		case 'b':
			decoded.WriteByte('\b')
			i++
		case 'f':
			decoded.WriteByte('\f')
			i++
		case 'n':
			decoded.WriteByte('\n')
			i++
		case 'r':
			decoded.WriteByte('\r')
			i++
		case 't':
			decoded.WriteByte('\t')
			i++
		case 'v':
			decoded.WriteByte('\v')
			i++
		case '0':
			decoded.WriteByte(0)
			i++
		case 'x':
			if value, next, ok := decodeFixedHex(raw, i+1, 2); ok {
				decoded.WriteRune(value)
				i = next
			} else {
				decoded.WriteByte('x')
				i++
			}
		case 'u':
			if i+1 < len(raw) && raw[i+1] == '{' {
				value, next, ok := decodeBracedHex(raw, i+2)
				if ok {
					decoded.WriteRune(value)
					i = next
				} else {
					decoded.WriteByte('u')
					i++
				}
			} else if value, next, ok := decodeFixedHex(raw, i+1, 4); ok {
				if value >= 0xd800 && value <= 0xdbff &&
					next+6 <= len(raw) && raw[next] == '\\' && raw[next+1] == 'u' {
					if low, lowNext, lowOK := decodeFixedHex(raw, next+2, 4); lowOK &&
						low >= 0xdc00 && low <= 0xdfff {
						value = 0x10000 + (value-0xd800)*0x400 + low - 0xdc00
						next = lowNext
					}
				}
				if value >= 0xd800 && value <= 0xdfff {
					value = '\ufffd'
				}
				decoded.WriteRune(value)
				i = next
			} else {
				decoded.WriteByte('u')
				i++
			}
		default:
			decoded.WriteByte(raw[i])
			i++
		}
	}
	return decoded.String()
}

func decodeCSSSpecifier(raw []byte) string {
	var decoded strings.Builder
	for i := 0; i < len(raw); {
		if raw[i] != '\\' {
			decoded.WriteByte(raw[i])
			i++
			continue
		}
		value, next, ok := decodeCSSEscape(raw, i)
		if !ok {
			decoded.WriteByte(raw[i])
			i++
			continue
		}
		if value != 0 {
			decoded.WriteRune(value)
		}
		i = next
	}
	return decoded.String()
}

func decodeCSSEscape(content []byte, slash int) (rune, int, bool) {
	if slash+1 >= len(content) || content[slash] != '\\' {
		return 0, slash, false
	}
	i := slash + 1
	if content[i] == '\n' || content[i] == '\f' {
		return 0, i + 1, true
	}
	if content[i] == '\r' {
		i++
		if i < len(content) && content[i] == '\n' {
			i++
		}
		return 0, i, true
	}
	if !isASCIIHex(content[i]) {
		value, width := utf8.DecodeRune(content[i:])
		return value, i + width, true
	}
	var value rune
	digits := 0
	for i < len(content) && digits < 6 && isASCIIHex(content[i]) {
		value = value*16 + rune(asciiHexValue(content[i]))
		i++
		digits++
	}
	if i < len(content) && isCSSSpace(content[i]) {
		if content[i] == '\r' && i+1 < len(content) && content[i+1] == '\n' {
			i++
		}
		i++
	}
	if value == 0 || value > 0x10ffff || value >= 0xd800 && value <= 0xdfff {
		value = '\ufffd'
	}
	return value, i, true
}

func decodeFixedHex(content []byte, start, count int) (rune, int, bool) {
	if start+count > len(content) {
		return 0, start, false
	}
	var value rune
	for i := start; i < start+count; i++ {
		if !isASCIIHex(content[i]) {
			return 0, start, false
		}
		value = value*16 + rune(asciiHexValue(content[i]))
	}
	return value, start + count, true
}

func decodeBracedHex(content []byte, start int) (rune, int, bool) {
	var value rune
	digits := 0
	for i := start; i < len(content); i++ {
		if content[i] == '}' {
			return value, i + 1, digits > 0 && digits <= 6 && value <= 0x10ffff
		}
		if !isASCIIHex(content[i]) || digits == 6 {
			return 0, start, false
		}
		value = value*16 + rune(asciiHexValue(content[i]))
		digits++
	}
	return 0, start, false
}

func jsKeywordAllowsExpression(word string) bool {
	switch word {
	case "await", "break", "case", "continue", "debugger", "default", "delete", "do",
		"else", "extends", "in", "instanceof", "new", "of", "return", "throw",
		"typeof", "void", "yield":
		return true
	default:
		return false
	}
}

func isJSControlHeader(word string) bool {
	switch word {
	case "catch", "for", "if", "switch", "while", "with":
		return true
	default:
		return false
	}
}

func isJSSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}

func skipJSSpaces(content []byte, start int) int {
	i := start
	for i < len(content) {
		if isJSSpace(content[i]) {
			i++
			continue
		}
		value, width := utf8.DecodeRune(content[i:])
		if !isJSUnicodeSpace(value) {
			break
		}
		i += width
	}
	return i
}

func isJSUnicodeSpace(value rune) bool {
	switch {
	case value == '\u00a0', value == '\u1680', value == '\u2028', value == '\u2029',
		value == '\u202f', value == '\u205f', value == '\u3000', value == '\ufeff':
		return true
	case value >= '\u2000' && value <= '\u200a':
		return true
	default:
		return false
	}
}

func isJSUnicodeLineTerminator(content []byte) bool {
	value, _ := utf8.DecodeRune(content)
	return value == '\u2028' || value == '\u2029'
}

func isCSSSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f':
		return true
	default:
		return false
	}
}

func isJSIdentifierStart(c byte) bool {
	return isASCIIAlpha(c) || c == '_' || c == '$' || c >= 0x80
}

func isJSIdentifierPart(c byte) bool {
	return isJSIdentifierStart(c) || isASCIIDigit(c)
}

func isCSSIdentifierStart(c byte) bool {
	return isASCIIAlpha(c) || c == '_' || c == '-' || c == '\\' || c >= 0x80
}

func isCSSIdentifierPart(c byte) bool {
	return isCSSIdentifierStart(c) || isASCIIDigit(c)
}

func isASCIIAlpha(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isASCIIHex(c byte) bool {
	return isASCIIDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func asciiHexValue(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

func isASCIIDigit(c byte) bool {
	return c >= '0' && c <= '9'
}
