package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>

void SetDockIcon(const void *data, int len) {
    NSData *imgData = [NSData dataWithBytes:data length:len];
    NSImage *img = [[NSImage alloc] initWithData:imgData];
    [NSApp setApplicationIconImage:img];
}
*/
import "C"
import "unsafe"

func setDockIcon(data []byte) {
	C.SetDockIcon(unsafe.Pointer(&data[0]), C.int(len(data)))
}
