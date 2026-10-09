package hostsys

// WASI errno values, as wasi-libc numbers them (__errno_values.h).
//
//declscope:shared // user.go, lock.go and path.go return them from host functions
const (
	errnoEACCES int32 = 2
	errnoEAGAIN int32 = 6
	errnoEBADF  int32 = 8
	errnoEINTR  int32 = 27
	errnoEINVAL int32 = 28
	errnoEIO    int32 = 29
	errnoENOENT int32 = 44
	errnoEPERM  int32 = 63
	errnoEROFS  int32 = 69
)
