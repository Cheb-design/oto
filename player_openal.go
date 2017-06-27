// Copyright 2015 Hajime Hoshi
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build darwin freebsd linux
// +build !js
// +build !android
// +build !ios

package oto

// #cgo darwin        LDFLAGS: -framework OpenAL
// #cgo freebsd linux LDFLAGS: -lopenal
//
// #ifdef __APPLE__
// #include <OpenAL/al.h>
// #include <OpenAL/alc.h>
// #else
// #include <AL/al.h>
// #include <AL/alc.h>
// #endif
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"
)

// As x/mobile/exp/audio/al is broken on macOS (https://github.com/golang/go/issues/15075),
// and that doesn't support FreeBSD, use OpenAL directly here.

type player struct {
	// alContext represents a pointer to ALCcontext. The type is uintptr since the value
	// can be 0x18 on macOS, which is invalid as a pointer value, and this might cause
	// GC errors.
	alContext   uintptr
	alDevice    uintptr
	alSource    C.ALuint
	sampleRate  int
	isClosed    bool
	alFormat    C.ALenum
	bufferUnits []C.ALuint
	tmpBuffer   []uint8
}

func alFormat(channelNum, bytesPerSample int) C.ALenum {
	switch {
	case channelNum == 1 && bytesPerSample == 1:
		return C.AL_FORMAT_MONO8
	case channelNum == 1 && bytesPerSample == 2:
		return C.AL_FORMAT_MONO16
	case channelNum == 2 && bytesPerSample == 1:
		return C.AL_FORMAT_STEREO8
	case channelNum == 2 && bytesPerSample == 2:
		return C.AL_FORMAT_STEREO16
	}
	panic(fmt.Sprintf("oto: invalid channel num (%d) or bytes per sample (%d)", channelNum, bytesPerSample))
}

func getError(device uintptr) error {
	c := C.alcGetError((*C.struct_ALCdevice_struct)(unsafe.Pointer(device)))
	switch c {
	case C.ALC_NO_ERROR:
		return nil
	case C.ALC_INVALID_DEVICE:
		return errors.New("OpenAL error: invalid device")
	case C.ALC_INVALID_CONTEXT:
		return errors.New("OpenAL error: invalid context")
	case C.ALC_INVALID_ENUM:
		return errors.New("OpenAL error: invalid enum")
	case C.ALC_INVALID_VALUE:
		return errors.New("OpenAL error: invalid value")
	case C.ALC_OUT_OF_MEMORY:
		return errors.New("OpenAL error: out of memory")
	default:
		return fmt.Errorf("OpenAL error: code %d", c)
	}
}

func newPlayer(sampleRate, channelNum, bytesPerSample, bufferSizeInBytes int) (*player, error) {
	name := C.alGetString(C.ALC_DEFAULT_DEVICE_SPECIFIER)
	d := uintptr(unsafe.Pointer(C.alcOpenDevice((*C.ALCchar)(name))))
	if d == 0 {
		return nil, fmt.Errorf("oto: alcOpenDevice must not return null")
	}
	c := uintptr(unsafe.Pointer(C.alcCreateContext((*C.struct_ALCdevice_struct)(unsafe.Pointer(d)), nil)))
	if c == 0 {
		return nil, fmt.Errorf("oto: alcCreateContext must not return null")
	}
	// Don't check getError until making the current context is done.
	// Linux might fail this check even though it succeeds (hajimehoshi/ebiten#204).
	C.alcMakeContextCurrent((*C.struct_ALCcontext_struct)(unsafe.Pointer(c)))
	if err := getError(d); err != nil {
		return nil, fmt.Errorf("oto: Activate: %v", err)
	}
	s := C.ALuint(0)
	C.alGenSources(1, &s)
	if err := getError(d); err != nil {
		return nil, fmt.Errorf("oto: NewSource: %v", err)
	}
	p := &player{
		alContext:   c,
		alDevice:    d,
		alSource:    s,
		sampleRate:  sampleRate,
		alFormat:    alFormat(channelNum, bytesPerSample),
		bufferUnits: make([]C.ALuint, bufferUnitNum(bufferSizeInBytes)),
	}
	runtime.SetFinalizer(p, (*player).Close)

	C.alGenBuffers(C.ALsizei(len(p.bufferUnits)), &p.bufferUnits[0])
	emptyBytes := make([]byte, bufferUnitSize)
	for _, b := range p.bufferUnits {
		// Note that the third argument of only the first buffer is used.
		C.alBufferData(b, p.alFormat, unsafe.Pointer(&emptyBytes[0]), C.ALsizei(bufferUnitSize), C.ALsizei(p.sampleRate))
	}
	C.alSourcePlay(p.alSource)
	if err := getError(d); err != nil {
		return nil, fmt.Errorf("oto: Play: %v", err)
	}
	return p, nil
}

func (p *player) Write(data []byte) (int, error) {
	if err := getError(p.alDevice); err != nil {
		return 0, fmt.Errorf("oto: starting Write: %v", err)
	}
	n := min(len(data), bufferUnitSize-len(p.tmpBuffer))
	p.tmpBuffer = append(p.tmpBuffer, data[:n]...)
	if len(p.tmpBuffer) < bufferUnitSize {
		return n, nil
	}
	pn := C.ALint(0)
	C.alGetSourcei(p.alSource, C.AL_BUFFERS_PROCESSED, &pn)
	if pn > 0 {
		bufs := make([]C.ALuint, pn)
		C.alSourceUnqueueBuffers(p.alSource, C.ALsizei(len(bufs)), &bufs[0])
		if err := getError(p.alDevice); err != nil {
			return 0, fmt.Errorf("oto: UnqueueBuffers: %v", err)
		}
		p.bufferUnits = append(p.bufferUnits, bufs...)
	}
	if len(p.bufferUnits) == 0 {
		return n, nil
	}
	bufferUnit := p.bufferUnits[0]
	p.bufferUnits = p.bufferUnits[1:]
	C.alBufferData(bufferUnit, p.alFormat, unsafe.Pointer(&p.tmpBuffer[0]), C.ALsizei(bufferUnitSize), C.ALsizei(p.sampleRate))
	C.alSourceQueueBuffers(p.alSource, 1, &bufferUnit)
	if err := getError(p.alDevice); err != nil {
		return 0, fmt.Errorf("oto: QueueBuffer: %v", err)
	}
	state := C.ALint(0)
	C.alGetSourcei(p.alSource, C.AL_SOURCE_STATE, &state)
	if state == C.AL_STOPPED || state == C.AL_INITIAL {
		C.alSourceRewind(p.alSource)
		C.alSourcePlay(p.alSource)
		if err := getError(p.alDevice); err != nil {
			return 0, fmt.Errorf("oto: Rewind or Play: %v", err)
		}
	}
	p.tmpBuffer = nil
	return n, nil
}

func (p *player) Close() error {
	if err := getError(p.alDevice); err != nil {
		return fmt.Errorf("oto: starting Close: %v", err)
	}
	if p.isClosed {
		return nil
	}
	var bs []C.ALuint
	C.alSourceRewind(p.alSource)
	C.alSourcePlay(p.alSource)
	n := C.ALint(0)
	C.alGetSourcei(p.alSource, C.AL_BUFFERS_QUEUED, &n)
	if 0 < n {
		bs = make([]C.ALuint, n)
		C.alSourceUnqueueBuffers(p.alSource, C.ALsizei(len(bs)), &bs[0])
		p.bufferUnits = append(p.bufferUnits, bs...)
	}
	C.alcCloseDevice((*C.struct_ALCdevice_struct)(unsafe.Pointer(p.alDevice)))
	p.isClosed = true
	if err := getError(p.alDevice); err != nil {
		return fmt.Errorf("oto: CloseDevice: %v", err)
	}
	runtime.SetFinalizer(p, nil)
	return nil
}
