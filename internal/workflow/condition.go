package workflow

import (
	"fmt"
	"strconv"
	"strings"
)

// Resolver supplies the value behind a reference at evaluation time. The
// engine wires this to upstream outputs, run inputs, and the secret source.
type Resolver func(Ref) (any, error)

// Expr is a parsed condition. The grammar is deliberately small: comparisons,
// boolean operators, parentheses, literals, and references. No function calls,
// no arbitrary code (.agents/spec/02-workflow-schema.md, "Conditional actions").
type Expr interface {
	// Eval computes the node's value using resolver to look up references.
	Eval(resolver Resolver) (any, error)
}

// Operator spellings. Constants rather than inline literals so the lexer, the
// parser, and the evaluator cannot drift apart on a typo.
const (
	opAnd = "&&"
	opOr  = "||"
	opEq  = "=="
	opNe  = "!="
	opLe  = "<="
	opGe  = ">="
	opLt  = "<"
	opGt  = ">"
	opNot = "!"
)

// ParseCondition turns a condition string into an evaluable expression. The
// returned error wraps ErrBadExpression with position context.
func ParseCondition(text string) (Expr, error) {
	tokens, err := lexCondition(text)
	if err != nil {
		return nil, err
	}

	parser := &condParser{tokens: tokens}

	expr, err := parser.parseOr()
	if err != nil {
		return nil, err
	}

	if parser.peek().kind != tokEOF {
		return nil, fmt.Errorf("%w: unexpected %q", ErrBadExpression, parser.peek().text)
	}

	return expr, nil
}

// EvalBool evaluates a condition to its final boolean. A non-boolean result
// is a type error: the grammar exists to gate actions, nothing else.
func EvalBool(expr Expr, resolver Resolver) (bool, error) {
	value, err := expr.Eval(resolver)
	if err != nil {
		return false, fmt.Errorf("condition: %w", err)
	}

	result, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%w: condition result is %T, want bool", ErrEvalType, value)
	}

	return result, nil
}

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokRef
	tokString
	tokNumber
	tokBool
	tokOp
	tokLParen
	tokRParen
)

type token struct {
	text    string
	ref     Ref
	kind    tokenKind
	num     float64
	boolVal bool
}

// lexCondition scans the condition text into tokens. References are lexed as
// single tokens so the parser never sees the delimiter syntax.
func lexCondition(text string) ([]token, error) {
	var tokens []token

	rest := text

	for {
		rest = strings.TrimLeft(rest, " \t\r\n")
		if rest == "" {
			tokens = append(tokens, token{kind: tokEOF})

			return tokens, nil
		}

		tok, remaining, err := lexOne(rest)
		if err != nil {
			return nil, err
		}

		tokens = append(tokens, tok)
		rest = remaining
	}
}

// lexOne consumes a single token from the front of rest.
func lexOne(rest string) (token, string, error) {
	if strings.HasPrefix(rest, refOpen) {
		return lexRef(rest)
	}

	if rest[0] == '\'' || rest[0] == '"' {
		return lexString(rest)
	}

	if isNumStart(rest) {
		return lexNumber(rest)
	}

	if word, remaining, found := lexWord(rest); found {
		return wordToken(word, remaining)
	}

	return lexOperator(rest)
}

func lexRef(rest string) (token, string, error) {
	closeAt := strings.Index(rest, refClose)
	if closeAt < 0 {
		return token{}, "", fmt.Errorf("%w: unterminated reference", ErrBadExpression)
	}

	raw := rest[:closeAt+len(refClose)]
	body := strings.TrimSpace(rest[len(refOpen):closeAt])

	ref, err := parseRef(raw, body)
	if err != nil {
		return token{}, "", err
	}

	return token{kind: tokRef, text: raw, ref: ref}, rest[closeAt+len(refClose):], nil
}

func lexString(rest string) (token, string, error) {
	quote := rest[0]

	closeAt := strings.IndexByte(rest[1:], quote)
	if closeAt < 0 {
		return token{}, "", fmt.Errorf("%w: unterminated string", ErrBadExpression)
	}

	return token{kind: tokString, text: rest[1 : closeAt+1]}, rest[closeAt+2:], nil
}

func isNumStart(rest string) bool {
	if isDigit(rest[0]) {
		return true
	}

	return rest[0] == '-' && len(rest) > 1 && isDigit(rest[1])
}

func lexNumber(rest string) (token, string, error) {
	length := 1
	for length < len(rest) && (isDigit(rest[length]) || rest[length] == '.') {
		length++
	}

	num, err := strconv.ParseFloat(rest[:length], 64)
	if err != nil {
		return token{}, "", fmt.Errorf("%w: bad number %q", ErrBadExpression, rest[:length])
	}

	return token{kind: tokNumber, text: rest[:length], num: num}, rest[length:], nil
}

func isDigit(char byte) bool {
	return char >= '0' && char <= '9'
}

func isWordByte(char byte) bool {
	if char == '_' || isDigit(char) {
		return true
	}

	return (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z')
}

func lexWord(rest string) (string, string, bool) {
	var length int
	for length < len(rest) && isWordByte(rest[length]) {
		length++
	}

	if length == 0 {
		return "", rest, false
	}

	return rest[:length], rest[length:], true
}

func wordToken(word, remaining string) (token, string, error) {
	switch word {
	case "true":
		return token{kind: tokBool, text: word, boolVal: true}, remaining, nil
	case "false":
		return token{kind: tokBool, text: word}, remaining, nil
	default:
		return token{}, "", fmt.Errorf("%w: unexpected word %q", ErrBadExpression, word)
	}
}

func lexOperator(rest string) (token, string, error) {
	// Two-byte operators come before single-byte ones so "<=" never lexes as
	// "<" followed by a stray "=".
	twoByte := []string{opAnd, opOr, opEq, opNe, opLe, opGe}
	for _, operator := range twoByte {
		if strings.HasPrefix(rest, operator) {
			return token{kind: tokOp, text: operator}, rest[len(operator):], nil
		}
	}

	switch rest[0] {
	case '<', '>', '!':
		return token{kind: tokOp, text: rest[:1]}, rest[1:], nil
	case '(':
		return token{kind: tokLParen, text: "("}, rest[1:], nil
	case ')':
		return token{kind: tokRParen, text: ")"}, rest[1:], nil
	default:
		return token{}, "", fmt.Errorf("%w: unexpected character %q", ErrBadExpression, rest[:1])
	}
}

type condParser struct {
	tokens []token
	pos    int
}

func (p *condParser) peek() token {
	return p.tokens[p.pos]
}

func (p *condParser) next() token {
	tok := p.tokens[p.pos]
	if tok.kind != tokEOF {
		p.pos++
	}

	return tok
}

func (p *condParser) parseOr() (Expr, error) {
	return p.parseBinary(opOr, p.parseAnd)
}

func (p *condParser) parseAnd() (Expr, error) {
	return p.parseBinary(opAnd, p.parseCompare)
}

// parseBinary handles one left-associative boolean precedence level.
func (p *condParser) parseBinary(operator string, nextLevel func() (Expr, error)) (Expr, error) {
	left, err := nextLevel()
	if err != nil {
		return nil, err
	}

	for p.peek().kind == tokOp && p.peek().text == operator {
		p.next()

		right, err := nextLevel()
		if err != nil {
			return nil, err
		}

		left = &binExpr{operator: operator, left: left, right: right}
	}

	return left, nil
}

func isCompareOp(text string) bool {
	switch text {
	case opEq, opNe, opLt, opLe, opGt, opGe:
		return true
	default:
		return false
	}
}

func (p *condParser) parseCompare() (Expr, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}

	if p.peek().kind != tokOp || !isCompareOp(p.peek().text) {
		return left, nil
	}

	operator := p.next().text

	right, err := p.parseUnary()
	if err != nil {
		return nil, err
	}

	return &binExpr{operator: operator, left: left, right: right}, nil
}

func (p *condParser) parseUnary() (Expr, error) {
	if p.peek().kind == tokOp && p.peek().text == opNot {
		p.next()

		inner, err := p.parseUnary()
		if err != nil {
			return nil, err
		}

		return &notExpr{inner: inner}, nil
	}

	return p.parsePrimary()
}

func (p *condParser) parsePrimary() (Expr, error) {
	tok := p.next()

	switch tok.kind {
	case tokLParen:
		return p.parseParen()
	case tokRef:
		return &refExpr{ref: tok.ref}, nil
	case tokString:
		return &litExpr{value: tok.text}, nil
	case tokNumber:
		return &litExpr{value: tok.num}, nil
	case tokBool:
		return &litExpr{value: tok.boolVal}, nil
	case tokOp, tokRParen, tokEOF:
		return nil, fmt.Errorf("%w: unexpected %q", ErrBadExpression, tok.text)
	default:
		return nil, fmt.Errorf("%w: unexpected token", ErrBadExpression)
	}
}

func (p *condParser) parseParen() (Expr, error) {
	inner, err := p.parseOr()
	if err != nil {
		return nil, err
	}

	if p.peek().kind != tokRParen {
		return nil, fmt.Errorf("%w: missing closing parenthesis", ErrBadExpression)
	}

	p.next()

	return inner, nil
}

type litExpr struct {
	value any
}

func (e *litExpr) Eval(_ Resolver) (any, error) {
	return e.value, nil
}

type refExpr struct {
	ref Ref
}

func (e *refExpr) Eval(resolver Resolver) (any, error) {
	if resolver == nil {
		return nil, fmt.Errorf("%w: %s", ErrUnresolvedRef, e.ref.Raw)
	}

	value, err := resolver(e.ref)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", e.ref.Raw, err)
	}

	return value, nil
}

type notExpr struct {
	inner Expr
}

func (e *notExpr) Eval(resolver Resolver) (any, error) {
	value, err := e.inner.Eval(resolver)
	if err != nil {
		return nil, fmt.Errorf("operand of %s: %w", opNot, err)
	}

	operand, ok := value.(bool)
	if !ok {
		return nil, fmt.Errorf("%w: operand of %s is %T, want bool", ErrEvalType, opNot, value)
	}

	return !operand, nil
}

type binExpr struct {
	left     Expr
	right    Expr
	operator string
}

func (e *binExpr) Eval(resolver Resolver) (any, error) {
	if e.operator == opAnd || e.operator == opOr {
		return e.evalBool(resolver)
	}

	return e.evalCompare(resolver)
}

// evalBool short-circuits the way the operators read: the right side never
// evaluates (and never resolves references) when the left side decides.
func (e *binExpr) evalBool(resolver Resolver) (any, error) {
	left, err := evalToBool(e.left, resolver, e.operator)
	if err != nil {
		return nil, err
	}

	if e.operator == opAnd && !left {
		return false, nil
	}

	if e.operator == opOr && left {
		return true, nil
	}

	return evalToBool(e.right, resolver, e.operator)
}

func evalToBool(expr Expr, resolver Resolver, operator string) (bool, error) {
	value, err := expr.Eval(resolver)
	if err != nil {
		return false, fmt.Errorf("operand of %s: %w", operator, err)
	}

	operand, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%w: operand of %s is %T, want bool", ErrEvalType, operator, value)
	}

	return operand, nil
}

func (e *binExpr) evalCompare(resolver Resolver) (any, error) {
	left, err := e.left.Eval(resolver)
	if err != nil {
		return nil, fmt.Errorf("left of %s: %w", e.operator, err)
	}

	right, err := e.right.Eval(resolver)
	if err != nil {
		return nil, fmt.Errorf("right of %s: %w", e.operator, err)
	}

	return compareValues(e.operator, left, right)
}

// compareValues implements the comparison table: numbers compare numerically
// (ints coerce to float64), strings compare lexically, bools support equality
// only. Anything else is a type error rather than a silent false.
func compareValues(operator string, left, right any) (any, error) {
	if leftNum, leftOK := toFloat(left); leftOK {
		rightNum, rightOK := toFloat(right)
		if !rightOK {
			return nil, typeMismatch(operator, left, right)
		}

		return compareOrdered(operator, leftNum, rightNum), nil
	}

	if leftStr, leftOK := left.(string); leftOK {
		rightStr, rightOK := right.(string)
		if !rightOK {
			return nil, typeMismatch(operator, left, right)
		}

		return compareOrdered(operator, leftStr, rightStr), nil
	}

	return compareBools(operator, left, right)
}

func compareBools(operator string, left, right any) (any, error) {
	leftBool, leftOK := left.(bool)

	rightBool, rightOK := right.(bool)
	if !leftOK || !rightOK {
		return nil, typeMismatch(operator, left, right)
	}

	switch operator {
	case opEq:
		return leftBool == rightBool, nil
	case opNe:
		return leftBool != rightBool, nil
	default:
		return nil, fmt.Errorf("%w: bools support %s and %s only, got %s", ErrEvalType, opEq, opNe, operator)
	}
}

func compareOrdered[T float64 | string](operator string, left, right T) bool {
	switch operator {
	case opEq:
		return left == right
	case opNe:
		return left != right
	case opLt:
		return left < right
	case opLe:
		return left <= right
	case opGt:
		return left > right
	default:
		return left >= right
	}
}

func toFloat(value any) (float64, bool) {
	switch num := value.(type) {
	case float64:
		return num, true
	case int64:
		return float64(num), true
	case int:
		return float64(num), true
	default:
		return 0, false
	}
}

func typeMismatch(operator string, left, right any) error {
	return fmt.Errorf("%w: cannot %s %T and %T", ErrEvalType, operator, left, right)
}
