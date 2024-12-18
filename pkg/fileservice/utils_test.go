// Copyright 2024 Matrix Origin
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fileservice

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestZeroToNil(t *testing.T) {
	if testing.AllocsPerRun(10, func() {
		if zeroToNil("") != nil {
			t.Fatal()
		}
		if zeroToNil("foo") == nil {
			t.Fatal()
		}
	}) != 0 {
		t.Fatal()
	}
}

func TestFirstNonZero(t *testing.T) {
	assert.Equal(t, 42, firstNonZero(0, 42))
	assert.Equal(t, 0, firstNonZero[int]())
}
