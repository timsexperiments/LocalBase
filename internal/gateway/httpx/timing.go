package httpx

import (
	"math"
	"time"
)

func Milliseconds(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func Round2(f float64) float64 { return math.Round(f*100) / 100 }
