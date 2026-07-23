package eval

import "testing"

func TestMathFunctions(t *testing.T) {
	g := testGraph(t)
	wantInt(t, g, "abs(-3)", 3)
	wantFloat(t, g, "abs(-2.5)", 2.5)
	wantFloat(t, g, "ceil(2.1)", 3.0)
	wantFloat(t, g, "floor(2.9)", 2.0)
	wantFloat(t, g, "round(2.5)", 3.0)
	wantFloat(t, g, "sqrt(9.0)", 3.0)
	wantInt(t, g, "sign(-5)", -1)
	wantInt(t, g, "sign(0)", 0)
	wantInt(t, g, "sign(7)", 1)
	wantNull(t, g, "sqrt('nope')")
}

func TestStringFunctions(t *testing.T) {
	g := testGraph(t)
	wantInt(t, g, "size('hello')", 5)
	wantStr(t, g, "left('hello', 3)", "hel")
	wantStr(t, g, "right('hello', 2)", "lo")
	wantStr(t, g, "substring('hello', 1, 3)", "ell")
	wantStr(t, g, "substring('hello', 2)", "llo")
	wantNull(t, g, "substring('hello', 1, -1)")
	wantStr(t, g, "'foo' + 'bar'", "foobar")
	wantBool(t, g, "'hello' STARTS WITH 'he'", true)
	wantBool(t, g, "'hello' ENDS WITH 'lo'", true)
	wantBool(t, g, "'hello' CONTAINS 'ell'", true)
	wantNull(t, g, "'hello' CONTAINS 3")
	wantStr(t, g, "lower('FootBALL')", "football")
	wantStr(t, g, "upper('FootBALL')", "FOOTBALL")
	wantStr(t, g, "toLower('FootBALL')", "football")
	wantStr(t, g, "toUpper('FootBALL')", "FOOTBALL")
	wantBool(t, g, "lower('The Football Cup') CONTAINS 'football'", true)
	wantNull(t, g, "lower(3)")
	wantNull(t, g, "upper(null)")
}

func TestConversionFunctions(t *testing.T) {
	g := testGraph(t)
	wantStr(t, g, "toString(42)", "42")
	wantStr(t, g, "toString(1.0)", "1.0")
	wantStr(t, g, "toString(true)", "true")
	wantStr(t, g, "toString(2.5)", "2.5")
	wantInt(t, g, "toInteger('3.9')", 3)
	wantInt(t, g, "toInteger(3.9)", 3)
	wantFloat(t, g, "toFloat('2.5')", 2.5)
	wantBool(t, g, "toBoolean('TRUE')", true)
	wantBool(t, g, "toBoolean('false')", false)
	wantNull(t, g, "toBoolean('nope')")
	wantNull(t, g, "toInteger('abc')")
	// toFloat promotes an int; toInteger truncates a negative float toward zero.
	wantFloat(t, g, "toFloat(3)", 3.0)
	wantInt(t, g, "toInteger(-3.9)", -3)
}

// TestListEndpointAndCountFunctions covers the collection builtins not already
// pinned elsewhere: cardinality and char_length counts, and the head/last
// endpoints (Null on an empty list).
func TestListEndpointAndCountFunctions(t *testing.T) {
	g := testGraph(t)
	wantInt(t, g, "cardinality([1, 2, 3, 4])", 4)
	wantInt(t, g, "char_length('hello')", 5)
	wantInt(t, g, "character_length('abcd')", 4)
	wantInt(t, g, "head([10, 20, 30])", 10)
	wantInt(t, g, "last([10, 20, 30])", 30)
	wantNull(t, g, "head([])")
}

// TestTrimAndCoalesceFunctions covers the whitespace trims and coalesce's
// first-non-null selection.
func TestTrimAndCoalesceFunctions(t *testing.T) {
	g := testGraph(t)
	wantStr(t, g, "trim('  hi  ')", "hi")
	wantStr(t, g, "ltrim('  hi')", "hi")
	wantStr(t, g, "rtrim('hi  ')", "hi")
	wantInt(t, g, "coalesce(null, null, 7)", 7)
	wantInt(t, g, "coalesce(3, 5)", 3)
	wantNull(t, g, "coalesce(null, null)")
}

// TestNormalizeBuiltins covers the NORMALIZE/is_normalized functions: ASCII
// text is already in every normalization form, and the one-argument NORMALIZE
// defaults to NFC.
func TestNormalizeBuiltins(t *testing.T) {
	g := testGraph(t)
	wantStr(t, g, "normalize('abc', NFC)", "abc")
	wantStr(t, g, "normalize('abc')", "abc")
	wantBool(t, g, "'abc' IS NORMALIZED", true)
	wantBool(t, g, "'abc' IS NOT NORMALIZED", false)
}
