package settings

import "github.com/basketikun/chatgpt2api/internal/pyval"

// Short local names for the shared Python conversions.
var (
	pyInt                = pyval.Int
	pyFloat              = pyval.Float
	pyTruthy             = pyval.Truthy
	pyStr                = pyval.Str
	normalizeBool        = pyval.NormalizeBool
	looseBool            = pyval.LooseBool
	normalizePositiveInt = pyval.PositiveInt
	asMap                = pyval.AsMap
)
