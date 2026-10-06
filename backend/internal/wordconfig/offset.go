package wordconfig

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

type offsetParser struct {
	input                 string
	index, depth, episode int
}

func evaluateOffset(expression string, episode int) (int, error) {
	if len(expression) > 128 || !strings.Contains(expression, "EP") {
		return 0, errors.New("invalid episode offset")
	}
	parser := offsetParser{input: expression, episode: episode}
	value, err := parser.add()
	if err != nil || parser.index != len(expression) || math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > 1e9 {
		return 0, errors.New("invalid episode offset")
	}
	return int(value), nil
}

func (parser *offsetParser) take(token string) bool {
	if strings.HasPrefix(parser.input[parser.index:], token) {
		parser.index += len(token)
		return true
	}
	return false
}

func (parser *offsetParser) add() (float64, error) {
	value, err := parser.multiply()
	for err == nil && parser.index < len(parser.input) {
		if parser.take("+") {
			var next float64
			next, err = parser.multiply()
			value += next
		} else if parser.take("-") {
			var next float64
			next, err = parser.multiply()
			value -= next
		} else {
			break
		}
	}
	return value, err
}

func (parser *offsetParser) multiply() (float64, error) {
	value, err := parser.unary()
	for err == nil && parser.index < len(parser.input) {
		floor := parser.take("//")
		divide := floor || parser.take("/")
		multiply := !divide && parser.take("*")
		if !divide && !multiply {
			break
		}
		var next float64
		next, err = parser.unary()
		if divide {
			if next == 0 {
				return 0, errors.New("episode offset division by zero")
			}
			value /= next
			if floor {
				value = math.Floor(value)
			}
		} else {
			value *= next
		}
	}
	return value, err
}

func (parser *offsetParser) unary() (float64, error) {
	parser.depth++
	defer func() { parser.depth-- }()
	if parser.depth > 32 {
		return 0, errors.New("episode offset too complex")
	}
	if parser.take("+") {
		return parser.unary()
	}
	if parser.take("-") {
		value, err := parser.unary()
		return -value, err
	}
	value, err := parser.atom()
	if err == nil && parser.take("**") {
		var power float64
		power, err = parser.unary()
		if math.Abs(power) > 64 {
			return 0, errors.New("episode offset exponent too large")
		}
		value = math.Pow(value, power)
	}
	return value, err
}

func (parser *offsetParser) atom() (float64, error) {
	if parser.take("EP") {
		return float64(parser.episode), nil
	}
	begin := parser.index
	for parser.index < len(parser.input) && parser.input[parser.index] >= '0' && parser.input[parser.index] <= '9' {
		parser.index++
	}
	if begin == parser.index {
		return 0, errors.New("invalid episode offset operand")
	}
	value, err := strconv.ParseFloat(parser.input[begin:parser.index], 64)
	if err != nil || value > 1e9 {
		return 0, errors.New("episode offset operand too large")
	}
	return value, nil
}
