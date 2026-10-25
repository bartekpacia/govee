// A Core Audio process tap copies the mix of everything the Mac is playing.
// macOS has no PulseAudio monitor, which is what parec records on Linux.
// The tap is available since macOS 14.2. Speakers stay unmuted.
// The first start asks for system-audio recording permission.

#import <CoreAudio/AudioHardware.h>
#import <CoreAudio/AudioHardwareTapping.h>
#import <CoreAudio/CATapDescription.h>
#import <Foundation/Foundation.h>

#include <errno.h>
#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

typedef struct {
	AudioObjectID tap;
	AudioObjectID agg;
	AudioDeviceIOProcID proc;
	int wfd;
	double srcRate;
	double phase;
	int nonInterleaved;
	int channels;
} Tap;

static Tap *gTap;

static char *status_str(const char *what, OSStatus s) {
	char fcc[5];
	uint32_t u = (uint32_t)s;
	fcc[0] = (u >> 24) & 0xff;
	fcc[1] = (u >> 16) & 0xff;
	fcc[2] = (u >> 8) & 0xff;
	fcc[3] = u & 0xff;
	fcc[4] = 0;
	int printable = 1;
	for (int i = 0; i < 4; i++) {
		if (fcc[i] < 32 || fcc[i] > 126) {
			printable = 0;
		}
	}
	char b[240];
	if (printable) {
		snprintf(b, sizeof b, "%s failed (OSStatus '%s' / %d)", what, fcc, (int)s);
	} else {
		snprintf(b, sizeof b, "%s failed (OSStatus %d)", what, (int)s);
	}
	return strdup(b);
}

// write_all writes p, retrying short writes and EINTR.
// EPIPE means the reader is gone; later audio is dropped.
static void write_all(int fd, const void *p, size_t n) {
	const char *b = p;
	while (n > 0) {
		ssize_t w = write(fd, b, n);
		if (w < 0) {
			if (errno == EINTR) {
				continue;
			}
			return;
		}
		b += w;
		n -= (size_t)w;
	}
}

// emit mixes to mono, resamples to 48 kHz, and writes s16le.
// Beat detection only needs the envelope, so linear interpolation is enough.
static void emit(Tap *t, const float *L, int strideL, const float *R, int strideR, UInt32 n) {
	if (n == 0 || t->srcRate <= 0) {
		return;
	}
	double step = t->srcRate / 48000.0;
	double phase = t->phase;
	int16_t buf[1024];
	int count = 0;
	while (phase < (double)n) {
		UInt32 i = (UInt32)phase;
		double frac = phase - (double)i;
		UInt32 j = i + 1 < n ? i + 1 : i;
		float l = L[i * strideL] + (L[j * strideL] - L[i * strideL]) * (float)frac;
		float r = R[i * strideR] + (R[j * strideR] - R[i * strideR]) * (float)frac;
		float mono = 0.5f * (l + r);
		if (mono > 1) {
			mono = 1;
		} else if (mono < -1) {
			mono = -1;
		}
		buf[count++] = (int16_t)lrintf(mono * 32767.0f);
		if (count == 1024) {
			write_all(t->wfd, buf, sizeof buf);
			count = 0;
		}
		phase += step;
	}
	if (count > 0) {
		write_all(t->wfd, buf, (size_t)count * sizeof buf[0]);
	}
	t->phase = phase - (double)n;
}

static OSStatus io_proc(AudioObjectID inDevice, const AudioTimeStamp *inNow,
	const AudioBufferList *inInputData, const AudioTimeStamp *inInputTime,
	AudioBufferList *outOutputData, const AudioTimeStamp *inOutputTime,
	void *inClientData) {
	(void)inDevice;
	(void)inNow;
	(void)inInputTime;
	(void)outOutputData;
	(void)inOutputTime;
	Tap *t = inClientData;
	if (inInputData == NULL || inInputData->mNumberBuffers < 1 || inInputData->mBuffers[0].mData == NULL) {
		return noErr;
	}
	const float *L = inInputData->mBuffers[0].mData;
	UInt32 n = inInputData->mBuffers[0].mDataByteSize / sizeof(float);
	if (t->nonInterleaved) {
		const float *R = L;
		if (t->channels > 1 && inInputData->mNumberBuffers > 1 && inInputData->mBuffers[1].mData != NULL) {
			R = inInputData->mBuffers[1].mData;
		}
		emit(t, L, 1, R, 1, n);
	} else {
		if (t->channels < 1) {
			return noErr;
		}
		n /= (UInt32)t->channels;
		const float *R = t->channels > 1 ? L + 1 : L;
		emit(t, L, t->channels, R, t->channels, n);
	}
	return noErr;
}

static void fail(Tap *t, int writeFD) {
	if (t != NULL) {
		if (t->proc && t->agg) {
			AudioDeviceDestroyIOProcID(t->agg, t->proc);
		}
		if (t->agg) {
			AudioHardwareDestroyAggregateDevice(t->agg);
		}
		if (t->tap) {
			AudioHardwareDestroyProcessTap(t->tap);
		}
		free(t);
	}
	if (writeFD >= 0) {
		close(writeFD);
	}
}

int tap_start(int writeFD, char **err) {
	*err = NULL;
	if (gTap != NULL) {
		close(writeFD);
		*err = strdup("already recording");
		return -1;
	}

	CATapDescription *desc = [[CATapDescription alloc] initStereoGlobalTapButExcludeProcesses:@[]];
	desc.name = @"govee";
	desc.privateTap = YES;
	desc.muteBehavior = CATapUnmuted;

	Tap *t = calloc(1, sizeof *t);
	t->wfd = writeFD;
	AudioObjectID tapID = kAudioObjectUnknown;
	OSStatus st = AudioHardwareCreateProcessTap(desc, &tapID);
	if (st != noErr) {
		*err = status_str("create process tap", st);
		char *hint = NULL;
		asprintf(&hint, "%s; allow this terminal in System Settings → Privacy & Security → Screen & System Audio Recording", *err);
		free(*err);
		*err = hint;
		fail(t, writeFD);
		return -1;
	}
	t->tap = tapID;

	AudioObjectPropertyAddress addr = {
		.mSelector = kAudioTapPropertyFormat,
		.mScope = kAudioObjectPropertyScopeGlobal,
		.mElement = kAudioObjectPropertyElementMain,
	};
	AudioStreamBasicDescription asbd = {0};
	UInt32 size = sizeof asbd;
	st = AudioObjectGetPropertyData(tapID, &addr, 0, NULL, &size, &asbd);
	if (st != noErr || asbd.mFormatID != kAudioFormatLinearPCM ||
		(asbd.mFormatFlags & kAudioFormatFlagIsFloat) == 0 || asbd.mBitsPerChannel != 32 ||
		asbd.mSampleRate <= 0 || asbd.mChannelsPerFrame < 1) {
		*err = st != noErr ? status_str("read tap format", st) : strdup("tap format is not 32-bit float");
		fail(t, writeFD);
		return -1;
	}
	t->srcRate = asbd.mSampleRate;
	t->channels = (int)asbd.mChannelsPerFrame;
	t->nonInterleaved = (asbd.mFormatFlags & kAudioFormatFlagIsNonInterleaved) != 0;

	NSDictionary *sub = @{
		[NSString stringWithUTF8String:kAudioSubTapUIDKey]: desc.UUID.UUIDString,
		[NSString stringWithUTF8String:kAudioSubTapDriftCompensationKey]: @YES,
	};
	NSDictionary *aggDesc = @{
		[NSString stringWithUTF8String:kAudioAggregateDeviceNameKey]: @"govee",
		[NSString stringWithUTF8String:kAudioAggregateDeviceUIDKey]: [NSUUID UUID].UUIDString,
		[NSString stringWithUTF8String:kAudioAggregateDeviceIsPrivateKey]: @YES,
		[NSString stringWithUTF8String:kAudioAggregateDeviceIsStackedKey]: @NO,
		[NSString stringWithUTF8String:kAudioAggregateDeviceTapAutoStartKey]: @YES,
		[NSString stringWithUTF8String:kAudioAggregateDeviceSubDeviceListKey]: @[],
		[NSString stringWithUTF8String:kAudioAggregateDeviceTapListKey]: @[sub],
	};
	AudioObjectID agg = kAudioObjectUnknown;
	st = AudioHardwareCreateAggregateDevice((__bridge CFDictionaryRef)aggDesc, &agg);
	if (st != noErr) {
		*err = status_str("create aggregate device", st);
		fail(t, writeFD);
		return -1;
	}
	t->agg = agg;

	AudioDeviceIOProcID proc = NULL;
	st = AudioDeviceCreateIOProcID(agg, io_proc, t, &proc);
	if (st != noErr) {
		*err = status_str("create IO proc", st);
		fail(t, writeFD);
		return -1;
	}
	t->proc = proc;
	st = AudioDeviceStart(agg, proc);
	if (st != noErr) {
		*err = status_str("start audio device", st);
		fail(t, writeFD);
		return -1;
	}
	gTap = t;
	return 0;
}

void tap_stop(void) {
	Tap *t = gTap;
	gTap = NULL;
	if (t == NULL) {
		return;
	}
	if (t->proc && t->agg) {
		AudioDeviceStop(t->agg, t->proc);
		AudioDeviceDestroyIOProcID(t->agg, t->proc);
	}
	if (t->agg) {
		AudioHardwareDestroyAggregateDevice(t->agg);
	}
	if (t->tap) {
		AudioHardwareDestroyProcessTap(t->tap);
	}
	if (t->wfd >= 0) {
		close(t->wfd);
	}
	free(t);
}
