// Copyright 2019 The Ebiten Authors
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

package audio

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hajimehoshi/ebiten/v2/internal/hooks"
)

// writerContext represents a context represented as io.WriteClosers.
// The actual implementation is oto.Context.
type writerContext interface {
	NewPlayer() io.WriteCloser
	io.Closer
}

var writerContextForTesting writerContext

func newWriterContext(sampleRate int) writerContext {
	if writerContextForTesting != nil {
		return writerContextForTesting
	}

	ch := make(chan struct{})
	var once sync.Once
	hooks.AppendHookOnBeforeUpdate(func() error {
		once.Do(func() {
			close(ch)
		})
		return nil
	})
	return newOtoContext(sampleRate, ch)
}

type writerContextPlayerImpl struct {
	context          *Context
	src              io.Reader
	sampleRate       int
	playing          bool
	closedExplicitly bool
	isLoopActive     bool

	buf     []byte
	readbuf []byte
	pos     int64
	volume  float64

	m sync.Mutex
}

func (p *writerContextPlayerImpl) Close() error {
	p.m.Lock()
	defer p.m.Unlock()

	p.playing = false
	if p.closedExplicitly {
		return fmt.Errorf("audio: the player is already closed")
	}
	p.closedExplicitly = true
	return nil
}

func (p *writerContextPlayerImpl) Play() {
	p.m.Lock()
	defer p.m.Unlock()

	if p.closedExplicitly {
		p.context.setError(fmt.Errorf("audio: the player is already closed"))
		return
	}

	p.playing = true
	if p.isLoopActive {
		return
	}

	// Set p.isLoopActive to true here, not in the loop. This prevents duplicated active loops.
	p.isLoopActive = true
	p.context.addPlayer(p)

	go p.loop()
	return
}

func (p *writerContextPlayerImpl) loop() {
	<-p.context.inited

	w := p.context.c.NewPlayer()
	wclosed := make(chan struct{})
	defer func() {
		<-wclosed
		w.Close()
	}()

	defer func() {
		p.m.Lock()
		p.playing = false
		p.context.removePlayer(p)
		p.isLoopActive = false
		p.m.Unlock()
	}()

	ch := make(chan []byte)
	defer close(ch)

	go func() {
		for buf := range ch {
			if _, err := w.Write(buf); err != nil {
				p.context.setError(err)
				break
			}
			p.context.setReady()
		}
		close(wclosed)
	}()

	for {
		buf, ok := p.read()
		if !ok {
			return
		}
		ch <- buf
	}
}

func (p *writerContextPlayerImpl) read() ([]byte, bool) {
	p.m.Lock()
	defer p.m.Unlock()

	if p.context.hasError() {
		return nil, false
	}

	if p.closedExplicitly {
		return nil, false
	}

	// playing can be false when pausing.
	if !p.playing {
		return nil, false
	}

	const bufSize = 2048

	p.context.semaphore <- struct{}{}
	defer func() {
		<-p.context.semaphore
	}()

	if p.readbuf == nil {
		p.readbuf = make([]byte, bufSize)
	}
	n, err := p.src.Read(p.readbuf[:bufSize-len(p.buf)])
	if err != nil {
		if err != io.EOF {
			p.context.setError(err)
			return nil, false
		}
		if n == 0 {
			return nil, false
		}
	}
	buf := append(p.buf, p.readbuf[:n]...)

	n2 := len(buf) - len(buf)%bytesPerSample
	buf, p.buf = buf[:n2], buf[n2:]

	for i := 0; i < len(buf)/2; i++ {
		v16 := int16(buf[2*i]) | (int16(buf[2*i+1]) << 8)
		v16 = int16(float64(v16) * p.volume)
		buf[2*i] = byte(v16)
		buf[2*i+1] = byte(v16 >> 8)
	}
	p.pos += int64(len(buf))

	return buf, true
}

func (p *writerContextPlayerImpl) IsPlaying() bool {
	p.m.Lock()
	r := p.playing
	p.m.Unlock()
	return r
}

func (p *writerContextPlayerImpl) Rewind() error {
	if _, ok := p.src.(io.Seeker); !ok {
		panic("audio: player to be rewound must be io.Seeker")
	}
	return p.Seek(0)
}

func (p *writerContextPlayerImpl) Seek(offset time.Duration) error {
	p.m.Lock()
	defer p.m.Unlock()

	o := int64(offset) * bytesPerSample * int64(p.sampleRate) / int64(time.Second)
	o = o - (o % bytesPerSample)

	seeker, ok := p.src.(io.Seeker)
	if !ok {
		panic("audio: the source must be io.Seeker when seeking")
	}
	pos, err := seeker.Seek(o, io.SeekStart)
	if err != nil {
		return err
	}

	p.buf = nil
	p.pos = pos
	return nil
}

func (p *writerContextPlayerImpl) Pause() {
	p.m.Lock()
	p.playing = false
	p.m.Unlock()
}

func (p *writerContextPlayerImpl) Current() time.Duration {
	p.m.Lock()
	sample := p.pos / bytesPerSample
	p.m.Unlock()
	return time.Duration(sample) * time.Second / time.Duration(p.sampleRate)
}

func (p *writerContextPlayerImpl) Volume() float64 {
	p.m.Lock()
	v := p.volume
	p.m.Unlock()
	return v
}

func (p *writerContextPlayerImpl) SetVolume(volume float64) {
	// The condition must be true when volume is NaN.
	if !(0 <= volume && volume <= 1) {
		panic("audio: volume must be in between 0 and 1")
	}

	p.m.Lock()
	p.volume = volume
	p.m.Unlock()
}

func (p *writerContextPlayerImpl) source() io.Reader {
	return p.src
}