// Expression AST nodes: the Expr interface, its node structs, operators,
// and parsed literals. Split from ast.go to keep files under the repo
// size norm.
package ast

// Expr is one expression node.
type Expr interface{ isExpr() }

// Lit is a literal constant.
type Lit struct{ Value Literal }

// Var is a bare variable reference.
type Var struct{ Name string }

// Prop is a property access on a variable, e.g. f.name.
type Prop struct {
	Var string
	Key string
}

// Unary is a prefix operator application.
type Unary struct {
	Op   UnOp
	Expr Expr
}

// Binary is an infix operator application.
type Binary struct {
	Op       BinOp
	LHS, RHS Expr
}

// Func is a function call; Star is count(*), Distinct is count(DISTINCT x).
type Func struct {
	Name     string
	Distinct bool
	Star     bool
	Args     []Expr
}

// ListExpr is a list literal.
type ListExpr struct{ Elems []Expr }

// In is `expr IN list`.
type In struct {
	Expr Expr
	List Expr
}

// IsNull is the postfix null test; Negated is the IS NOT NULL form.
type IsNull struct {
	Expr    Expr
	Negated bool
}

// IsTruth is the postfix truth-value test `x IS [NOT] TRUE|FALSE`:
// IS TRUE is true iff x is the boolean true (null and non-booleans read
// false); IS FALSE iff x is the boolean false. NOT negates the whole
// test. (IS UNKNOWN parses to IsNull -- unknown IS the null truth value.)
type IsTruth struct {
	Expr    Expr
	Want    bool
	Negated bool
}

// IsTyped is `x IS [NOT] TYPED <type>`: a runtime value-kind test.
type IsTyped struct {
	Expr    Expr
	Kind    string // normalized lowercase: integer, float, string, boolean, list, node, relationship
	Negated bool
}

// CaseWhen is one WHEN cond THEN result arm.
type CaseWhen struct {
	Cond   Expr
	Result Expr
}

// Case is CASE [operand] WHEN...THEN... [ELSE default] END. A non-nil
// Operand is the simple form (each cond compares to the operand);
// otherwise the searched form (each cond is a boolean).
type Case struct {
	Operand Expr // nil for the searched form
	Whens   []CaseWhen
	Else    Expr // nil when absent
}

// Cost is the weighted shortest-path cost between two bound endpoints
// (engine node; no GQL surface syntax -- reachable via CALL procedures).
type Cost struct {
	From   string
	To     string
	Types  []string
	Dir    Dir
	Weight CostSpec
}

// Exists is EXISTS { MATCH pattern [WHERE expr] }: true if the (possibly
// correlated) inner pattern matches at least once for the current row.
type Exists struct {
	Pattern *Pattern
	Where   Expr // nil when absent
}

// CountSub is COUNT { MATCH pattern [WHERE expr] }: how many times the
// correlated inner pattern matches the current row.
type CountSub struct {
	Pattern *Pattern
	Where   Expr // nil when absent
}

// Quant is the list-predicate quantifier of a ListPred.
type Quant uint8

const (
	// QuantAll is all(...).
	QuantAll Quant = iota
	// QuantAny is any(...).
	QuantAny
	// QuantNone is none(...).
	QuantNone
	// QuantSingle is single(...).
	QuantSingle
)

// ListPred is a list-predicate quantifier: all(x IN list WHERE pred) and
// the any/none/single forms.
type ListPred struct {
	Quant Quant
	Var   string
	List  Expr
	Pred  Expr
}

// Reduce is reduce(acc = init, x IN list | body): left-fold a list
// (engine node; no GQL surface syntax yet).
type Reduce struct {
	Acc  string
	Init Expr
	Var  string
	List Expr
	Body Expr
}

// ListComp is [x IN list WHERE filter | map] (engine node; no GQL surface
// syntax yet).
type ListComp struct {
	Var    string
	List   Expr
	Filter Expr // nil when absent
	Map    Expr // nil when absent
}

// PatternComp is [ (pattern) [WHERE filter] | proj ] (engine node; no GQL
// surface syntax yet).
type PatternComp struct {
	Pattern *Pattern
	Where   Expr // nil when absent
	Proj    Expr
}

// Index is base[index] (0-based; negative counts from the end; out of
// range yields null).
type Index struct {
	Base Expr
	Idx  Expr
}

// Slice is base[from..to]; either bound may be nil; negative bounds count
// from the end.
type Slice struct {
	Base Expr
	From Expr // nil when omitted
	To   Expr // nil when omitted
}

// PropOf is property access on an arbitrary expression (rels(e)[i].ts);
// the common ident.key parses as Prop.
type PropOf struct {
	Base Expr
	Key  string
}

// MapProjEntryKind discriminates a map-projection entry.
type MapProjEntryKind uint8

const (
	// MapProjProp is .key.
	MapProjProp MapProjEntryKind = iota
	// MapProjField is name: expr.
	MapProjField
	// MapProjAll is .* (all properties).
	MapProjAll
)

// MapProjEntry is one entry of a MapProj.
type MapProjEntry struct {
	Kind MapProjEntryKind
	Key  string
	Expr Expr // MapProjField only
}

// MapProj is a map projection var{.key, alias: expr, .*} (engine node; no
// GQL surface syntax yet).
type MapProj struct {
	Var     string
	Entries []MapProjEntry
}

// MapField is one key/value of a MapLit, in written order.
type MapField struct {
	Key string
	Val Expr
}

// MapLit is a map literal {key: expr, ...} in expression position; values
// are arbitrary expressions (unlike a pattern's inline property map).
type MapLit struct{ Fields []MapField }

// HasLabelExpr tests a LabelExpr against the node bound to Var -- built
// from the surface label predicate (n:Label) and by the planner lowering a
// node pattern's general label expression.
type HasLabelExpr struct {
	Var  string
	Expr *LabelExpr
}

func (*IsTruth) isExpr()      {}
func (*IsTyped) isExpr()      {}
func (*Lit) isExpr()          {}
func (*Var) isExpr()          {}
func (*Prop) isExpr()         {}
func (*Unary) isExpr()        {}
func (*Binary) isExpr()       {}
func (*Func) isExpr()         {}
func (*ListExpr) isExpr()     {}
func (*In) isExpr()           {}
func (*IsNull) isExpr()       {}
func (*Case) isExpr()         {}
func (*Cost) isExpr()         {}
func (*Exists) isExpr()       {}
func (*CountSub) isExpr()     {}
func (*ListPred) isExpr()     {}
func (*Reduce) isExpr()       {}
func (*ListComp) isExpr()     {}
func (*PatternComp) isExpr()  {}
func (*Index) isExpr()        {}
func (*Slice) isExpr()        {}
func (*PropOf) isExpr()       {}
func (*MapProj) isExpr()      {}
func (*MapLit) isExpr()       {}
func (*HasLabelExpr) isExpr() {}

// UnOp is a prefix operator.
type UnOp uint8

const (
	// Not is boolean negation.
	Not UnOp = iota
	// Neg is arithmetic negation.
	Neg
)

// BinOp is an infix operator.
type BinOp uint8

const (
	// OpOr is OR.
	OpOr BinOp = iota
	// OpAnd is AND.
	OpAnd
	// OpEq is =.
	OpEq
	// OpNeq is <>.
	OpNeq
	// OpLt is <.
	OpLt
	// OpLte is <=.
	OpLte
	// OpGt is >.
	OpGt
	// OpGte is >=.
	OpGte
	// OpAdd is +.
	OpAdd
	// OpSub is -.
	OpSub
	// OpMul is *.
	OpMul
	// OpDiv is /.
	OpDiv
	// OpStartsWith is STARTS WITH (comparison precedence).
	OpStartsWith
	// OpEndsWith is ENDS WITH.
	OpEndsWith
	// OpContains is CONTAINS.
	OpContains
	// OpXor is XOR (three-valued: null when either side is null).
	OpXor
	// OpConcat is || -- string/list concatenation only (null otherwise;
	// unlike +, it never adds numbers).
	OpConcat
)

// LitKind discriminates a Literal.
type LitKind uint8

const (
	// LitInt is an integer literal.
	LitInt LitKind = iota
	// LitFloat is a float literal.
	LitFloat
	// LitStr is a string literal.
	LitStr
	// LitBool is a boolean literal.
	LitBool
	// LitNull is null.
	LitNull
	// LitParam is an auto-lifted parameter slot -- never produced by the
	// parser; the autoparam pass replaces constants so queries differing
	// only in a constant share one cached plan.
	LitParam
	// LitNamedParam is an explicit $name parameter.
	LitNamedParam
)

// Literal is a parsed constant (distinct from a runtime value).
type Literal struct {
	Kind LitKind
	I    int64
	F    float64
	S    string // LitStr text or LitNamedParam name
	B    bool
	P    uint32 // LitParam slot
}

// IntLit is an integer literal.
func IntLit(v int64) Literal { return Literal{Kind: LitInt, I: v} }

// FloatLit is a float literal.
func FloatLit(v float64) Literal { return Literal{Kind: LitFloat, F: v} }

// StrLit is a string literal.
func StrLit(s string) Literal { return Literal{Kind: LitStr, S: s} }

// BoolLit is a boolean literal.
func BoolLit(b bool) Literal { return Literal{Kind: LitBool, B: b} }

// NullLit is the null literal.
func NullLit() Literal { return Literal{Kind: LitNull} }

// ParamLit is an auto-lifted parameter slot.
func ParamLit(slot uint32) Literal { return Literal{Kind: LitParam, P: slot} }

// NamedParamLit is a $name parameter.
func NamedParamLit(name string) Literal { return Literal{Kind: LitNamedParam, S: name} }
