// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// 垃圾回收器：写屏障（write barrier）
//
// 对于并发 GC，go 编译器通过调用 write barrier 来实现对可能在堆对象中的指针值字段更新。
// 单个指针写入的主要 write barrier 是 gcWriteBarrier，并在汇编中实现。
// 此文件包含 bulk 操作的 write barrier，另见 mwbbuf.go。

package runtime

import (
	"runtime/internal/sys"
	"unsafe"
)

// Go 使用了一个混合屏障（hybrid barrier），它结合了 Yuasa 风格的
// 删除屏障（deletion barrier）[对需要覆盖其引用的对象进行着色] 和
// Dijkstra 风格的插入屏障（insertion barrier）[对正在写入其引用的对象进行着色]。
// 调用的 goroutine 栈是灰色时，插入部分的 barrier 是必要的，伪代码如下：
//
//     writePointer(slot, ptr):
//         shade(*slot)
//         if current stack is grey:
//             shade(ptr)
//         *slot = ptr
//
// 在 Go 代码中：
//   slot 是 dst
//   ptr 是进入 slot 的 src
//
// shade 表示 通过对 wbuf 添加引用并标记它已经观察到了白色指针。
//
// 两个 shade 以及条件语句一起工作来防止 mutator 隐藏垃圾搜集器中的对象：
//
// 1. shade(*slot) 通过将一个指针从堆移动到栈 来防止 mutator 一个对象。
//    如果它视图从堆中取消链接，则会进行着色。
//
// 2. shade(ptr) 通过将一个对象从栈移动到堆的黑色对象来防止 mutator 隐藏对象。
//    如果它尝试将指针安插在黑色对象中，将对其着色。
//
// 3. 一旦 goroutine 栈为黑色，shade(ptr) 便不必要了。shade(ptr) 通过将对象从栈
//    移动到堆来防止隐藏对象，但这需要首先在堆上隐藏指针。扫描堆后，它立即只指向着色对象，
//    因此它不会隐藏任何内容，并且 shade(*slot) 可以防止它隐藏其堆上任何其他指针。
//
// 关于此屏障的细节描述和正确性证明，请参考：https://github.com/golang/proposal/blob/master/design/17503-eliminate-rescan.md
//
//
//
// 处理内存顺序:
//
// Yuasa和Dijkstra屏障都可以以包含槽的对象的颜色为条件。
// 我们选择不进行这些条件化，因为确保持有槽的对象不会在没有 mutator 注意的情况下
// 同时改变颜色的成本似乎过高。
//
// 考虑以下示例，其中 mutator 写入插槽然后加载插槽的标记位，
// 而 GC 线程写入插槽的标记位，然后作为扫描的一部分读取插槽。
//
// 初始状态下 [slot] 和 [slotmark] 均为 0 (nil)
// Mutator 线程          GC 线程
// st [slot], ptr       st [slotmark], 1
//
// ld r1, [slotmark]       ld r2, [slot]
//
// Without an expensive memory barrier between the st and the ld, the final
// result on most HW (including 386/amd64) can be r1==r2==0. This is a classic
// example of what can happen when loads are allowed to be reordered with older
// stores (avoiding such reorderings lies at the heart of the classic
// Peterson/Dekker algorithms for mutual exclusion). Rather than require memory
// barriers, which will slow down both the mutator and the GC, we always grey
// the ptr object regardless of the slot's color.
//
// Another place where we intentionally omit memory barriers is when
// accessing mheap_.arena_used to check if a pointer points into the
// heap. On relaxed memory machines, it's possible for a mutator to
// extend the size of the heap by updating arena_used, allocate an
// object from this new region, and publish a pointer to that object,
// but for tracing running on another processor to observe the pointer
// but use the old value of arena_used. In this case, tracing will not
// mark the object, even though it's reachable. However, the mutator
// is guaranteed to execute a write barrier when it publishes the
// pointer, so it will take care of marking the object. A general
// consequence of this is that the garbage collector may cache the
// value of mheap_.arena_used. (See issue #9984.)
//
//
// 栈写入(stack writes):
//
// The compiler omits write barriers for writes to the current frame,
// but if a stack pointer has been passed down the call stack, the
// compiler will generate a write barrier for writes through that
// pointer (because it doesn't know it's not a heap pointer).
//
// One might be tempted to ignore the write barrier if slot points
// into to the stack. Don't do it! Mark termination only re-scans
// frames that have potentially been active since the concurrent scan,
// so it depends on write barriers to track changes to pointers in
// stack frames that have not been active.
//
//
// 全局写(global writes):
//
// The Go garbage collector requires write barriers when heap pointers
// are stored in globals. Many garbage collectors ignore writes to
// globals and instead pick up global -> heap pointers during
// termination. This increases pause time, so we instead rely on write
// barriers for writes to globals so that we don't have to rescan
// global during mark termination.
//
//
// 公开顺序(publication ordering):
//
// The write barrier is *pre-publication*, meaning that the write
// barrier happens prior to the *slot = ptr write that may make ptr
// reachable by some goroutine that currently cannot reach it.
//
//
// 信号处理指针写入:
//
// 通常，信号处理程序无法安全地调用写屏障，因为它可以在没有 P 的情况下运行，甚至在写屏障期间也可以运行。
//
// 只有一个例外：profbuf.go 在信号处理程序配置文件记录期间省略了一个障碍。
// 这只是因为删除障碍而安全。有关详细参数，请参阅 profbuf.go。
// 如果我们移除删除障碍，我们将必须找到一种新方法来处理配置文件记录。

// typedmemmove copies a value of type t to dst from src.
// Must be nosplit, see #16026.
//
// TODO: Perfect for go:nosplitrec since we can't have a safe point
// anywhere in the bulk barrier or memmove.
//
//go:nosplit
func typedmemmove(typ *_type, dst, src unsafe.Pointer) {
	if dst == src {
		return
	}
	if typ.kind&kindNoPointers == 0 {
		bulkBarrierPreWrite(uintptr(dst), uintptr(src), typ.size)
	}
	// There's a race here: if some other goroutine can write to
	// src, it may change some pointer in src after we've
	// performed the write barrier but before we perform the
	// memory copy. This safe because the write performed by that
	// other goroutine must also be accompanied by a write
	// barrier, so at worst we've unnecessarily greyed the old
	// pointer that was in src.
	memmove(dst, src, typ.size)
	if writeBarrier.cgo {
		cgoCheckMemmove(typ, dst, src, 0, typ.size)
	}
}

//go:linkname reflect_typedmemmove reflect.typedmemmove
func reflect_typedmemmove(typ *_type, dst, src unsafe.Pointer) {
	if raceenabled {
		raceWriteObjectPC(typ, dst, getcallerpc(), funcPC(reflect_typedmemmove))
		raceReadObjectPC(typ, src, getcallerpc(), funcPC(reflect_typedmemmove))
	}
	if msanenabled {
		msanwrite(dst, typ.size)
		msanread(src, typ.size)
	}
	typedmemmove(typ, dst, src)
}

// typedmemmovepartial is like typedmemmove but assumes that
// dst and src point off bytes into the value and only copies size bytes.
//go:linkname reflect_typedmemmovepartial reflect.typedmemmovepartial
func reflect_typedmemmovepartial(typ *_type, dst, src unsafe.Pointer, off, size uintptr) {
	if writeBarrier.needed && typ.kind&kindNoPointers == 0 && size >= sys.PtrSize {
		// Pointer-align start address for bulk barrier.
		adst, asrc, asize := dst, src, size
		if frag := -off & (sys.PtrSize - 1); frag != 0 {
			adst = add(dst, frag)
			asrc = add(src, frag)
			asize -= frag
		}
		bulkBarrierPreWrite(uintptr(adst), uintptr(asrc), asize&^(sys.PtrSize-1))
	}

	memmove(dst, src, size)
	if writeBarrier.cgo {
		cgoCheckMemmove(typ, dst, src, off, size)
	}
}

// reflectcallmove is invoked by reflectcall to copy the return values
// out of the stack and into the heap, invoking the necessary write
// barriers. dst, src, and size describe the return value area to
// copy. typ describes the entire frame (not just the return values).
// typ may be nil, which indicates write barriers are not needed.
//
// It must be nosplit and must only call nosplit functions because the
// stack map of reflectcall is wrong.
//
//go:nosplit
func reflectcallmove(typ *_type, dst, src unsafe.Pointer, size uintptr) {
	if writeBarrier.needed && typ != nil && typ.kind&kindNoPointers == 0 && size >= sys.PtrSize {
		bulkBarrierPreWrite(uintptr(dst), uintptr(src), size)
	}
	memmove(dst, src, size)
}

//go:nosplit
func typedslicecopy(typ *_type, dst, src slice) int {
	n := dst.len
	if n > src.len {
		n = src.len
	}
	if n == 0 {
		return 0
	}
	dstp := dst.array
	srcp := src.array

	// The compiler emits calls to typedslicecopy before
	// instrumentation runs, so unlike the other copying and
	// assignment operations, it's not instrumented in the calling
	// code and needs its own instrumentation.
	if raceenabled {
		callerpc := getcallerpc()
		pc := funcPC(slicecopy)
		racewriterangepc(dstp, uintptr(n)*typ.size, callerpc, pc)
		racereadrangepc(srcp, uintptr(n)*typ.size, callerpc, pc)
	}
	if msanenabled {
		msanwrite(dstp, uintptr(n)*typ.size)
		msanread(srcp, uintptr(n)*typ.size)
	}

	if writeBarrier.cgo {
		cgoCheckSliceCopy(typ, dst, src, n)
	}

	if dstp == srcp {
		return n
	}

	// Note: No point in checking typ.kind&kindNoPointers here:
	// compiler only emits calls to typedslicecopy for types with pointers,
	// and growslice and reflect_typedslicecopy check for pointers
	// before calling typedslicecopy.
	size := uintptr(n) * typ.size
	if writeBarrier.needed {
		bulkBarrierPreWrite(uintptr(dstp), uintptr(srcp), size)
	}
	// See typedmemmove for a discussion of the race between the
	// barrier and memmove.
	memmove(dstp, srcp, size)
	return n
}

//go:linkname reflect_typedslicecopy reflect.typedslicecopy
func reflect_typedslicecopy(elemType *_type, dst, src slice) int {
	if elemType.kind&kindNoPointers != 0 {
		n := dst.len
		if n > src.len {
			n = src.len
		}
		if n == 0 {
			return 0
		}

		size := uintptr(n) * elemType.size
		if raceenabled {
			callerpc := getcallerpc()
			pc := funcPC(reflect_typedslicecopy)
			racewriterangepc(dst.array, size, callerpc, pc)
			racereadrangepc(src.array, size, callerpc, pc)
		}
		if msanenabled {
			msanwrite(dst.array, size)
			msanread(src.array, size)
		}

		memmove(dst.array, src.array, size)
		return n
	}
	return typedslicecopy(elemType, dst, src)
}

// typedmemclr clears the typed memory at ptr with type typ. The
// memory at ptr must already be initialized (and hence in type-safe
// state). If the memory is being initialized for the first time, see
// memclrNoHeapPointers.
//
// If the caller knows that typ has pointers, it can alternatively
// call memclrHasPointers.
//
//go:nosplit
func typedmemclr(typ *_type, ptr unsafe.Pointer) {
	if typ.kind&kindNoPointers == 0 {
		bulkBarrierPreWrite(uintptr(ptr), 0, typ.size)
	}
	memclrNoHeapPointers(ptr, typ.size)
}

//go:linkname reflect_typedmemclr reflect.typedmemclr
func reflect_typedmemclr(typ *_type, ptr unsafe.Pointer) {
	typedmemclr(typ, ptr)
}

//go:linkname reflect_typedmemclrpartial reflect.typedmemclrpartial
func reflect_typedmemclrpartial(typ *_type, ptr unsafe.Pointer, off, size uintptr) {
	if typ.kind&kindNoPointers == 0 {
		bulkBarrierPreWrite(uintptr(ptr), 0, size)
	}
	memclrNoHeapPointers(ptr, size)
}

//go:linkname reflect_typedmemclr reflect.typedmemclr
func reflect_typedmemclr(typ *_type, ptr unsafe.Pointer) {
	typedmemclr(typ, ptr)
}

//go:linkname reflect_typedmemclrpartial reflect.typedmemclrpartial
func reflect_typedmemclrpartial(typ *_type, ptr unsafe.Pointer, off, size uintptr) {
	if typ.kind&kindNoPointers == 0 {
		bulkBarrierPreWrite(uintptr(ptr), 0, size)
	}
	memclrNoHeapPointers(ptr, size)
}

// memclrHasPointers clears n bytes of typed memory starting at ptr.
// The caller must ensure that the type of the object at ptr has
// pointers, usually by checking typ.kind&kindNoPointers. However, ptr
// does not have to point to the start of the allocation.
//
//go:nosplit
func memclrHasPointers(ptr unsafe.Pointer, n uintptr) {
	bulkBarrierPreWrite(uintptr(ptr), 0, n)
	memclrNoHeapPointers(ptr, n)
}
