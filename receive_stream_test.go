package quic

import (
	"errors"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go/internal/mocks"
	"github.com/quic-go/quic-go/internal/protocol"
	"github.com/quic-go/quic-go/internal/wire"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"go.uber.org/mock/gomock"
)

var _ = Describe("Receive Stream", func() {
	const streamID protocol.StreamID = 1337

	var (
		str            *receiveStream
		strWithTimeout io.Reader // str wrapped with gbytes.TimeoutReader
		mockFC         *mocks.MockStreamFlowController
		mockSender     *MockStreamSender
	)

	BeforeEach(func() {
		mockSender = NewMockStreamSender(mockCtrl)
		mockFC = mocks.NewMockStreamFlowController(mockCtrl)
		str = newReceiveStream(streamID, mockSender, mockFC)

		timeout := scaleDuration(250 * time.Millisecond)
		strWithTimeout = gbytes.TimeoutReader(str, timeout)
	})

	It("gets stream id", func() {
		Expect(str.StreamID()).To(Equal(protocol.StreamID(1337)))
	})

	Context("reading", func() {
		It("reads a single STREAM frame", func() {
			now := time.Now()
			mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(4), false, now)
			mockFC.EXPECT().AddBytesRead(protocol.ByteCount(4))
			frame := wire.StreamFrame{
				Offset: 0,
				Data:   []byte{0xDE, 0xAD, 0xBE, 0xEF},
			}
			Expect(str.handleStreamFrame(&frame, now)).To(Succeed())
			b := make([]byte, 4)
			n, err := strWithTimeout.Read(b)
			Expect(err).ToNot(HaveOccurred())
			Expect(n).To(Equal(4))
			Expect(b).To(Equal([]byte{0xDE, 0xAD, 0xBE, 0xEF}))
		})

		It("reads a single STREAM frame in multiple goes", func() {
			mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(4), false, gomock.Any())
			mockFC.EXPECT().AddBytesRead(protocol.ByteCount(2))
			mockFC.EXPECT().AddBytesRead(protocol.ByteCount(2))
			frame := wire.StreamFrame{
				Offset: 0,
				Data:   []byte{0xDE, 0xAD, 0xBE, 0xEF},
			}
			Expect(str.handleStreamFrame(&frame, time.Now())).To(Succeed())
			b := make([]byte, 2)
			n, err := strWithTimeout.Read(b)
			Expect(err).ToNot(HaveOccurred())
			Expect(n).To(Equal(2))
			Expect(b).To(Equal([]byte{0xDE, 0xAD}))
			n, err = strWithTimeout.Read(b)
			Expect(err).ToNot(HaveOccurred())
			Expect(n).To(Equal(2))
			Expect(b).To(Equal([]byte{0xBE, 0xEF}))
		})

		It("queues a flow control update", func() {
			now := time.Now()
			mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(4), false, now)
			mockFC.EXPECT().AddBytesRead(protocol.ByteCount(3)).Return(true)
			frame := wire.StreamFrame{
				Offset: 0,
				Data:   []byte{0xde, 0xad, 0xbe, 0xef},
			}
			Expect(str.handleStreamFrame(&frame, now)).To(Succeed())
			mockSender.EXPECT().onHasStreamControlFrame(streamID, str)
			n, err := strWithTimeout.Read(make([]byte, 3))
			Expect(err).ToNot(HaveOccurred())
			Expect(n).To(Equal(3))
			now = now.Add(time.Second)
			mockFC.EXPECT().GetWindowUpdate(now).Return(protocol.ByteCount(1337))
			f, ok, hasMore := str.getControlFrame(now)
			Expect(ok).To(BeTrue())
			Expect(f.Frame).To(Equal(&wire.MaxStreamDataFrame{StreamID: streamID, MaximumStreamData: 1337}))
			Expect(hasMore).To(BeFalse())
		})

		It("reads all data available", func() {
			mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(2), false, gomock.Any())
			mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(4), false, gomock.Any())
			mockFC.EXPECT().AddBytesRead(protocol.ByteCount(2)).Times(2)
			frame1 := wire.StreamFrame{
				Offset: 0,
				Data:   []byte{0xDE, 0xAD},
			}
			frame2 := wire.StreamFrame{
				Offset: 2,
				Data:   []byte{0xBE, 0xEF},
			}
			Expect(str.handleStreamFrame(&frame1, time.Now())).To(Succeed())
			Expect(str.handleStreamFrame(&frame2, time.Now())).To(Succeed())
			b := make([]byte, 6)
			n, err := strWithTimeout.Read(b)
			Expect(err).ToNot(HaveOccurred())
			Expect(n).To(Equal(4))
			Expect(b).To(Equal([]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x00}))
		})

		It("assembles multiple STREAM frames", func() {
			mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(2), false, gomock.Any())
			mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(4), false, gomock.Any())
			mockFC.EXPECT().AddBytesRead(protocol.ByteCount(2)).Times(2)
			frame1 := wire.StreamFrame{
				Offset: 0,
				Data:   []byte{0xDE, 0xAD},
			}
			frame2 := wire.StreamFrame{
				Offset: 2,
				Data:   []byte{0xBE, 0xEF},
			}
			Expect(str.handleStreamFrame(&frame1, time.Now())).To(Succeed())
			Expect(str.handleStreamFrame(&frame2, time.Now())).To(Succeed())
			b := make([]byte, 4)
			n, err := strWithTimeout.Read(b)
			Expect(err).ToNot(HaveOccurred())
			Expect(n).To(Equal(4))
			Expect(b).To(Equal([]byte{0xDE, 0xAD, 0xBE, 0xEF}))
		})

		It("waits until data is available", func() {
			mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(2), false, gomock.Any())
			mockFC.EXPECT().AddBytesRead(protocol.ByteCount(2))
			go func() {
				defer GinkgoRecover()
				frame := wire.StreamFrame{Data: []byte{0xDE, 0xAD}}
				time.Sleep(10 * time.Millisecond)
				Expect(str.handleStreamFrame(&frame, time.Now())).To(Succeed())
			}()
			b := make([]byte, 2)
			n, err := strWithTimeout.Read(b)
			Expect(err).ToNot(HaveOccurred())
			Expect(n).To(Equal(2))
		})

		It("handles STREAM frames in wrong order", func() {
			mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(2), false, gomock.Any())
			mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(4), false, gomock.Any())
			mockFC.EXPECT().AddBytesRead(protocol.ByteCount(2)).Times(2)
			frame1 := wire.StreamFrame{
				Offset: 2,
				Data:   []byte{0xBE, 0xEF},
			}
			frame2 := wire.StreamFrame{
				Offset: 0,
				Data:   []byte{0xDE, 0xAD},
			}
			Expect(str.handleStreamFrame(&frame1, time.Now())).To(Succeed())
			Expect(str.handleStreamFrame(&frame2, time.Now())).To(Succeed())
			b := make([]byte, 4)
			n, err := strWithTimeout.Read(b)
			Expect(err).ToNot(HaveOccurred())
			Expect(n).To(Equal(4))
			Expect(b).To(Equal([]byte{0xDE, 0xAD, 0xBE, 0xEF}))
		})

		It("ignores duplicate STREAM frames", func() {
			mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(2), false, gomock.Any())
			mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(2), false, gomock.Any())
			mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(4), false, gomock.Any())
			mockFC.EXPECT().AddBytesRead(protocol.ByteCount(2)).Times(2)
			frame1 := wire.StreamFrame{
				Offset: 0,
				Data:   []byte{0xDE, 0xAD},
			}
			frame2 := wire.StreamFrame{
				Offset: 0,
				Data:   []byte{0x13, 0x37},
			}
			frame3 := wire.StreamFrame{
				Offset: 2,
				Data:   []byte{0xBE, 0xEF},
			}
			Expect(str.handleStreamFrame(&frame1, time.Now())).To(Succeed())
			Expect(str.handleStreamFrame(&frame2, time.Now())).To(Succeed())
			Expect(str.handleStreamFrame(&frame3, time.Now())).To(Succeed())
			b := make([]byte, 4)
			n, err := strWithTimeout.Read(b)
			Expect(err).ToNot(HaveOccurred())
			Expect(n).To(Equal(4))
			Expect(b).To(Equal([]byte{0xDE, 0xAD, 0xBE, 0xEF}))
		})

		It("doesn't rejects a STREAM frames with an overlapping data range", func() {
			mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(4), false, gomock.Any())
			mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(6), false, gomock.Any())
			mockFC.EXPECT().AddBytesRead(protocol.ByteCount(2))
			mockFC.EXPECT().AddBytesRead(protocol.ByteCount(4))
			frame1 := wire.StreamFrame{
				Offset: 0,
				Data:   []byte("foob"),
			}
			frame2 := wire.StreamFrame{
				Offset: 2,
				Data:   []byte("obar"),
			}
			Expect(str.handleStreamFrame(&frame1, time.Now())).To(Succeed())
			Expect(str.handleStreamFrame(&frame2, time.Now())).To(Succeed())
			b := make([]byte, 6)
			n, err := strWithTimeout.Read(b)
			Expect(err).ToNot(HaveOccurred())
			Expect(n).To(Equal(6))
			Expect(b).To(Equal([]byte("foobar")))
		})

		Context("deadlines", func() {
			It("the deadline error has the right net.Error properties", func() {
				Expect(errDeadline.Timeout()).To(BeTrue())
				Expect(errDeadline).To(MatchError("deadline exceeded"))
			})

			It("returns an error when Read is called after the deadline", func() {
				mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(6), false, gomock.Any()).AnyTimes()
				Expect(str.handleStreamFrame(&wire.StreamFrame{Data: []byte("foobar")}, time.Now())).To(Succeed())
				str.SetReadDeadline(time.Now().Add(-time.Second))
				b := make([]byte, 6)
				n, err := strWithTimeout.Read(b)
				Expect(err).To(MatchError(errDeadline))
				Expect(n).To(BeZero())
			})

			It("unblocks when the deadline is changed to the past", func() {
				str.SetReadDeadline(time.Now().Add(time.Hour))
				done := make(chan struct{})
				go func() {
					defer GinkgoRecover()
					_, err := str.Read(make([]byte, 6))
					Expect(err).To(MatchError(errDeadline))
					close(done)
				}()
				Consistently(done).ShouldNot(BeClosed())
				str.SetReadDeadline(time.Now().Add(-time.Hour))
				Eventually(done).Should(BeClosed())
			})

			It("unblocks after the deadline", func() {
				deadline := time.Now().Add(scaleDuration(50 * time.Millisecond))
				str.SetReadDeadline(deadline)
				b := make([]byte, 6)
				n, err := strWithTimeout.Read(b)
				Expect(err).To(MatchError(errDeadline))
				Expect(n).To(BeZero())
				Expect(time.Now()).To(BeTemporally("~", deadline, scaleDuration(20*time.Millisecond)))
			})

			It("doesn't unblock if the deadline is changed before the first one expires", func() {
				deadline1 := time.Now().Add(scaleDuration(50 * time.Millisecond))
				deadline2 := time.Now().Add(scaleDuration(100 * time.Millisecond))
				str.SetReadDeadline(deadline1)
				go func() {
					defer GinkgoRecover()
					time.Sleep(scaleDuration(20 * time.Millisecond))
					str.SetReadDeadline(deadline2)
					// make sure that this was actually execute before the deadline expires
					Expect(time.Now()).To(BeTemporally("<", deadline1))
				}()
				runtime.Gosched()
				b := make([]byte, 10)
				n, err := strWithTimeout.Read(b)
				Expect(err).To(MatchError(errDeadline))
				Expect(n).To(BeZero())
				Expect(time.Now()).To(BeTemporally("~", deadline2, scaleDuration(20*time.Millisecond)))
			})

			It("unblocks earlier, when a new deadline is set", func() {
				deadline1 := time.Now().Add(scaleDuration(200 * time.Millisecond))
				deadline2 := time.Now().Add(scaleDuration(50 * time.Millisecond))
				go func() {
					defer GinkgoRecover()
					time.Sleep(scaleDuration(10 * time.Millisecond))
					str.SetReadDeadline(deadline2)
					// make sure that this was actually execute before the deadline expires
					Expect(time.Now()).To(BeTemporally("<", deadline2))
				}()
				str.SetReadDeadline(deadline1)
				runtime.Gosched()
				b := make([]byte, 10)
				_, err := strWithTimeout.Read(b)
				Expect(err).To(MatchError(errDeadline))
				Expect(time.Now()).To(BeTemporally("~", deadline2, scaleDuration(25*time.Millisecond)))
			})

			It("doesn't unblock if the deadline is removed", func() {
				deadline := time.Now().Add(scaleDuration(50 * time.Millisecond))
				str.SetReadDeadline(deadline)
				deadlineUnset := make(chan struct{})
				go func() {
					defer GinkgoRecover()
					time.Sleep(scaleDuration(20 * time.Millisecond))
					str.SetReadDeadline(time.Time{})
					// make sure that this was actually execute before the deadline expires
					Expect(time.Now()).To(BeTemporally("<", deadline))
					close(deadlineUnset)
				}()
				done := make(chan struct{})
				go func() {
					defer GinkgoRecover()
					_, err := strWithTimeout.Read(make([]byte, 1))
					Expect(err).To(MatchError("test done"))
					close(done)
				}()
				runtime.Gosched()
				Eventually(deadlineUnset).Should(BeClosed())
				Consistently(done, scaleDuration(100*time.Millisecond)).ShouldNot(BeClosed())
				// make the go routine return
				str.closeForShutdown(errors.New("test done"))
				Eventually(done).Should(BeClosed())
			})
		})

		Context("closing", func() {
			Context("with FIN bit", func() {
				It("returns EOFs", func() {
					mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(4), true, gomock.Any())
					mockFC.EXPECT().AddBytesRead(protocol.ByteCount(4))
					Expect(str.handleStreamFrame(&wire.StreamFrame{
						Offset: 0,
						Data:   []byte{0xDE, 0xAD, 0xBE, 0xEF},
						Fin:    true,
					}, time.Now())).To(Succeed())
					mockSender.EXPECT().onStreamCompleted(streamID)
					b := make([]byte, 4)
					n, err := strWithTimeout.Read(b)
					Expect(err).To(MatchError(io.EOF))
					Expect(n).To(Equal(4))
					Expect(b).To(Equal([]byte{0xDE, 0xAD, 0xBE, 0xEF}))
					n, err = strWithTimeout.Read(b)
					Expect(n).To(BeZero())
					Expect(err).To(MatchError(io.EOF))
				})

				It("handles out-of-order frames", func() {
					mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(2), false, gomock.Any())
					mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(4), true, gomock.Any())
					mockFC.EXPECT().AddBytesRead(protocol.ByteCount(2)).Times(2)
					frame1 := wire.StreamFrame{
						Offset: 2,
						Data:   []byte{0xBE, 0xEF},
						Fin:    true,
					}
					frame2 := wire.StreamFrame{
						Offset: 0,
						Data:   []byte{0xDE, 0xAD},
					}
					Expect(str.handleStreamFrame(&frame1, time.Now())).To(Succeed())
					Expect(str.handleStreamFrame(&frame2, time.Now())).To(Succeed())
					mockSender.EXPECT().onStreamCompleted(streamID)
					b := make([]byte, 4)
					n, err := strWithTimeout.Read(b)
					Expect(err).To(MatchError(io.EOF))
					Expect(n).To(Equal(4))
					Expect(b).To(Equal([]byte{0xDE, 0xAD, 0xBE, 0xEF}))
					n, err = strWithTimeout.Read(b)
					Expect(n).To(BeZero())
					Expect(err).To(MatchError(io.EOF))
				})

				It("returns EOFs with partial read", func() {
					mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(2), true, gomock.Any())
					mockFC.EXPECT().AddBytesRead(protocol.ByteCount(2))
					Expect(str.handleStreamFrame(&wire.StreamFrame{
						Offset: 0,
						Data:   []byte{0xde, 0xad},
						Fin:    true,
					}, time.Now())).To(Succeed())
					mockSender.EXPECT().onStreamCompleted(streamID)
					b := make([]byte, 4)
					n, err := strWithTimeout.Read(b)
					Expect(err).To(MatchError(io.EOF))
					Expect(n).To(Equal(2))
					Expect(b[:n]).To(Equal([]byte{0xde, 0xad}))
				})

				It("handles immediate FINs", func() {
					mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(0), true, gomock.Any())
					mockFC.EXPECT().AddBytesRead(protocol.ByteCount(0))
					Expect(str.handleStreamFrame(&wire.StreamFrame{
						Offset: 0,
						Fin:    true,
					}, time.Now())).To(Succeed())
					mockSender.EXPECT().onStreamCompleted(streamID)
					b := make([]byte, 4)
					n, err := strWithTimeout.Read(b)
					Expect(n).To(BeZero())
					Expect(err).To(MatchError(io.EOF))
				})

				// Calling Read concurrently doesn't make any sense (and is forbidden),
				// but we still want to make sure that we don't complete the stream more than once
				// if the user misuses our API.
				// This would lead to an INTERNAL_ERROR ("tried to delete unknown outgoing stream"),
				// which can be hard to debug.
				// Note that even without the protection built into the receiveStream, this test
				// is very timing-dependent, and would need to run a few hundred times to trigger the failure.
				It("handles concurrent reads", func() {
					mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(6), gomock.Any(), gomock.Any()).AnyTimes()
					var bytesRead protocol.ByteCount
					mockFC.EXPECT().AddBytesRead(gomock.Any()).Do(func(n protocol.ByteCount) bool {
						bytesRead += n
						return false
					}).AnyTimes()

					var numCompleted int32
					mockSender.EXPECT().onStreamCompleted(streamID).Do(func(protocol.StreamID) {
						atomic.AddInt32(&numCompleted, 1)
					}).AnyTimes()
					const num = 3
					var wg sync.WaitGroup
					wg.Add(num)
					for i := 0; i < num; i++ {
						go func() {
							defer wg.Done()
							defer GinkgoRecover()
							_, err := str.Read(make([]byte, 8))
							Expect(err).To(MatchError(io.EOF))
						}()
					}
					Expect(str.handleStreamFrame(&wire.StreamFrame{
						Offset: 0,
						Data:   []byte("foobar"),
						Fin:    true,
					}, time.Now())).To(Succeed())
					wg.Wait()
					Expect(bytesRead).To(BeEquivalentTo(6))
					Expect(atomic.LoadInt32(&numCompleted)).To(BeEquivalentTo(1))
				})
			})
		})

		Context("closing for shutdown", func() {
			testErr := errors.New("test error")

			It("immediately returns all reads", func() {
				done := make(chan struct{})
				b := make([]byte, 4)
				go func() {
					defer GinkgoRecover()
					n, err := strWithTimeout.Read(b)
					Expect(n).To(BeZero())
					Expect(err).To(MatchError(testErr))
					close(done)
				}()
				Consistently(done).ShouldNot(BeClosed())
				str.closeForShutdown(testErr)
				Eventually(done).Should(BeClosed())
			})

			It("errors for all following reads", func() {
				str.closeForShutdown(testErr)
				b := make([]byte, 1)
				n, err := strWithTimeout.Read(b)
				Expect(n).To(BeZero())
				Expect(err).To(MatchError(testErr))
			})
		})
	})

	Context("stream cancellations", func() {
		Context("canceling read", func() {
			It("unblocks Read", func() {
				mockSender.EXPECT().onHasStreamControlFrame(gomock.Any(), gomock.Any())
				done := make(chan struct{})
				go func() {
					defer GinkgoRecover()
					_, err := strWithTimeout.Read([]byte{0})
					Expect(err).To(Equal(&StreamError{
						StreamID:  streamID,
						ErrorCode: 1234,
						Remote:    false,
					}))
					close(done)
				}()
				Consistently(done).ShouldNot(BeClosed())
				str.CancelRead(1234)
				Eventually(done).Should(BeClosed())
			})

			It("doesn't allow further calls to Read", func() {
				mockSender.EXPECT().onHasStreamControlFrame(gomock.Any(), gomock.Any())
				str.CancelRead(1234)
				_, err := strWithTimeout.Read([]byte{0})
				Expect(err).To(Equal(&StreamError{
					StreamID:  streamID,
					ErrorCode: 1234,
					Remote:    false,
				}))
			})

			It("does nothing when CancelRead is called twice", func() {
				mockSender.EXPECT().onHasStreamControlFrame(gomock.Any(), gomock.Any())
				str.CancelRead(1234)
				str.CancelRead(1234)
				_, err := strWithTimeout.Read([]byte{0})
				Expect(err).To(Equal(&StreamError{
					StreamID:  streamID,
					ErrorCode: 1234,
					Remote:    false,
				}))
			})

			It("queues a STOP_SENDING frame", func() {
				mockSender.EXPECT().onHasStreamControlFrame(gomock.Any(), gomock.Any())
				str.CancelRead(1234)
				f, ok, hasMore := str.getControlFrame(time.Now())
				Expect(ok).To(BeTrue())
				Expect(f.Frame).To(Equal(&wire.StopSendingFrame{
					StreamID:  streamID,
					ErrorCode: 1234,
				}))
				Expect(hasMore).To(BeFalse())
			})

			It("doesn't send a STOP_SENDING frame, if the FIN was already read", func() {
				mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(6), true, gomock.Any())
				mockFC.EXPECT().AddBytesRead(protocol.ByteCount(6))
				// no calls to mockSender.queueControlFrame
				Expect(str.handleStreamFrame(&wire.StreamFrame{
					StreamID: streamID,
					Data:     []byte("foobar"),
					Fin:      true,
				}, time.Now())).To(Succeed())
				mockSender.EXPECT().onStreamCompleted(streamID)
				n, err := strWithTimeout.Read(make([]byte, 100))
				Expect(err).To(MatchError(io.EOF))
				Expect(n).To(Equal(6))
				str.CancelRead(1234)
			})

			It("doesn't send a STOP_SENDING frame, if the stream was already reset", func() {
				mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(42), true, gomock.Any())
				mockFC.EXPECT().Abandon().MinTimes(1)
				Expect(str.handleResetStreamFrame(&wire.ResetStreamFrame{
					ErrorCode: 1337,
					StreamID:  streamID,
					FinalSize: 42,
				}, time.Now())).To(Succeed())
				mockSender.EXPECT().onStreamCompleted(gomock.Any())
				str.CancelRead(1234)
				// check that the error indicates a remote reset
				n, err := str.Read([]byte{0})
				Expect(err).To(HaveOccurred())
				Expect(n).To(BeZero())
				var streamErr *StreamError
				Expect(errors.As(err, &streamErr)).To(BeTrue())
				Expect(streamErr.ErrorCode).To(BeEquivalentTo(1337))
				Expect(streamErr.Remote).To(BeTrue())
			})

			It("sends a STOP_SENDING after receiving the final offset", func() {
				mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(6), true, gomock.Any())
				Expect(str.handleStreamFrame(&wire.StreamFrame{
					Data: []byte("foobar"),
					Fin:  true,
				}, time.Now())).To(Succeed())
				mockFC.EXPECT().Abandon()
				mockSender.EXPECT().onHasStreamControlFrame(gomock.Any(), gomock.Any())
				mockSender.EXPECT().onStreamCompleted(streamID)
				str.CancelRead(1234)
				// read the error
				n, err := str.Read([]byte{0})
				Expect(err).To(HaveOccurred())
				Expect(n).To(BeZero())
			})

			It("completes the stream when receiving the Fin after the stream was canceled", func() {
				mockSender.EXPECT().onHasStreamControlFrame(gomock.Any(), gomock.Any())
				str.CancelRead(1234)
				gomock.InOrder(
					mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(1000), true, gomock.Any()),
					mockFC.EXPECT().Abandon(),
				)
				mockSender.EXPECT().onStreamCompleted(streamID)
				Expect(str.handleStreamFrame(&wire.StreamFrame{
					Offset: 1000,
					Fin:    true,
				}, time.Now())).To(Succeed())
			})

			It("handles duplicate FinBits after the stream was canceled", func() {
				mockSender.EXPECT().onHasStreamControlFrame(gomock.Any(), gomock.Any())
				str.CancelRead(1234)
				gomock.InOrder(
					mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(1000), true, gomock.Any()),
					mockFC.EXPECT().Abandon(),
					mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(1000), true, gomock.Any()),
				)
				mockSender.EXPECT().onStreamCompleted(streamID)
				Expect(str.handleStreamFrame(&wire.StreamFrame{
					Offset: 1000,
					Fin:    true,
				}, time.Now())).To(Succeed())
				Expect(str.handleStreamFrame(&wire.StreamFrame{
					Offset: 1000,
					Fin:    true,
				}, time.Now())).To(Succeed())
			})

			It("ignores cancellations after closeForShutdown", func() {
				closeErr := errors.New("closed for shutdown")
				str.closeForShutdown(closeErr)
				buf := make([]byte, 100)
				_, err := str.Read(buf)
				Expect(err).To(Equal(closeErr))
				str.CancelRead(42)
				_, err = str.Read(buf)
				Expect(err).To(Equal(closeErr))
			})
		})

		Context("receiving RESET_STREAM frames", func() {
			rst := &wire.ResetStreamFrame{
				StreamID:  streamID,
				FinalSize: 42,
				ErrorCode: 1234,
			}

			It("unblocks Read", func() {
				done := make(chan struct{})
				go func() {
					defer GinkgoRecover()
					_, err := strWithTimeout.Read([]byte{0})
					Expect(err).To(Equal(&StreamError{
						StreamID:  streamID,
						ErrorCode: 1234,
						Remote:    true,
					}))
					close(done)
				}()
				Consistently(done).ShouldNot(BeClosed())
				mockSender.EXPECT().onStreamCompleted(streamID)
				gomock.InOrder(
					mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(42), true, gomock.Any()),
					mockFC.EXPECT().Abandon(),
				)
				Expect(str.handleResetStreamFrame(rst, time.Now())).To(Succeed())
				Eventually(done).Should(BeClosed())
			})

			It("doesn't allow further calls to Read", func() {
				mockSender.EXPECT().onStreamCompleted(streamID)
				gomock.InOrder(
					mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(42), true, gomock.Any()),
					mockFC.EXPECT().Abandon(),
				)
				Expect(str.handleResetStreamFrame(rst, time.Now())).To(Succeed())
				_, err := strWithTimeout.Read([]byte{0})
				Expect(err).To(MatchError(&StreamError{
					StreamID:  streamID,
					ErrorCode: 1234,
				}))
			})

			It("errors when receiving a RESET_STREAM with an inconsistent offset", func() {
				testErr := errors.New("already received a different final offset before")
				mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(42), true, gomock.Any()).Return(testErr)
				Expect(str.handleResetStreamFrame(rst, time.Now())).To(MatchError(testErr))
			})

			It("ignores duplicate RESET_STREAM frames", func() {
				mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(42), true, gomock.Any()).Times(2)
				mockFC.EXPECT().Abandon()
				Expect(str.handleResetStreamFrame(rst, time.Now())).To(Succeed())
				Expect(str.handleResetStreamFrame(rst, time.Now())).To(Succeed())
			})

			It("doesn't call onStreamCompleted again when the final offset was already received via Fin", func() {
				mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(42), true, gomock.Any()).Times(2)
				Expect(str.handleStreamFrame(&wire.StreamFrame{
					StreamID: streamID,
					Offset:   rst.FinalSize,
					Fin:      true,
				}, time.Now())).To(Succeed())
				mockFC.EXPECT().Abandon().MinTimes(1)
				mockSender.EXPECT().onStreamCompleted(streamID)
				Expect(str.handleResetStreamFrame(rst, time.Now())).To(Succeed())
				// now read the error
				n, err := str.Read([]byte{0})
				Expect(err).To(HaveOccurred())
				Expect(n).To(BeZero())
			})

			It("doesn't do anything when it was closed for shutdown", func() {
				str.closeForShutdown(errors.New("shutdown"))
				Expect(str.handleResetStreamFrame(rst, time.Now())).To(Succeed())
			})

			It("handles RESET_STREAM after CancelRead", func() {
				mockFC.EXPECT().Abandon()
				mockSender.EXPECT().onHasStreamControlFrame(gomock.Any(), gomock.Any())
				str.CancelRead(1234)
				mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(42), true, gomock.Any())
				mockSender.EXPECT().onStreamCompleted(streamID)
				Expect(str.handleResetStreamFrame(rst, time.Now())).To(Succeed())
				// check that the error indicates a local reset
				n, err := str.Read([]byte{0})
				Expect(err).To(HaveOccurred())
				Expect(n).To(BeZero())
				var streamErr *StreamError
				Expect(errors.As(err, &streamErr)).To(BeTrue())
				Expect(streamErr.Remote).To(BeFalse())
			})
		})
	})

	It("errors when a STREAM frame causes a flow control violation", func() {
		testErr := errors.New("flow control violation")
		mockFC.EXPECT().UpdateHighestReceived(protocol.ByteCount(8), false, gomock.Any()).Return(testErr)
		Expect(str.handleStreamFrame(&wire.StreamFrame{
			Offset: 2,
			Data:   []byte("foobar"),
		}, time.Now())).To(MatchError(testErr))
	})
})
