// Native macOS resource sampling for the Go-owned Mini Fabrics installer.
// Build on macOS with: xcrun swiftc -O -framework Foundation -framework Metal
// SystemInfo.swift -o fabrics-system-info
import Foundation
import Darwin
import Metal

enum ProbeError: Error, CustomStringConvertible {
    case mach(String, kern_return_t)

    var description: String {
        switch self {
        case let .mach(operation, code):
            return "\(operation) failed (Mach status \(code))"
        }
    }
}

struct SystemInfo: Encodable {
    let schema_version = 1
    let ram_total_bytes: UInt64
    let ram_available_bytes: UInt64
    let ram_available_kind = "free_including_speculative_plus_inactive"
    let vram_total_bytes: UInt64
    let vram_available_bytes: UInt64
    let gpu_memory_kind = "metal_recommended_working_set"
    let gpu_available_kind = "recommended_budget_minus_helper_allocations_not_global_free_vram"
    let gpu_memory_is_estimate = true
    let unified_memory: Bool
    let metal_available: Bool
    let cpu_arch: String
    let os_version: String
    let gpu_name: String
}

func architecture() -> String {
    var name = utsname()
    guard uname(&name) == 0 else { return "unknown" }
    let capacity = MemoryLayout.size(ofValue: name.machine)
    return withUnsafePointer(to: &name.machine) { pointer in
        pointer.withMemoryRebound(to: CChar.self, capacity: capacity) {
            String(cString: $0)
        }
    }
}

@available(macOS 10.15, *)
func sample() throws -> SystemInfo {
    let host = mach_host_self()
    defer { _ = mach_port_deallocate(mach_task_self_, host) }
    var pageSize: vm_size_t = 0
    var status = host_page_size(host, &pageSize)
    guard status == KERN_SUCCESS else { throw ProbeError.mach("host_page_size", status) }
    var statistics = vm_statistics64()
    var count = mach_msg_type_number_t(MemoryLayout<vm_statistics64>.stride / MemoryLayout<integer_t>.stride)
    status = withUnsafeMutablePointer(to: &statistics) { pointer in
        pointer.withMemoryRebound(to: integer_t.self, capacity: Int(count)) {
            host_statistics64(host, HOST_VM_INFO64, $0, &count)
        }
    }
    guard status == KERN_SUCCESS else { throw ProbeError.mach("host_statistics64", status) }

    let total = ProcessInfo.processInfo.physicalMemory
    // Darwin's free_count already includes speculative pages. Do not add
    // speculative_count again, or add purgeable pages that overlap VM queues.
    // Inactive pages are reclaimable estimates; this is not an allocation promise.
    let available = min(total, (UInt64(statistics.free_count) + UInt64(statistics.inactive_count)) * UInt64(pageSize))
    let device = MTLCreateSystemDefaultDevice()
    let budget = device?.recommendedMaxWorkingSetSize ?? 0
    let allocated = UInt64(device?.currentAllocatedSize ?? 0)
    let gpuAvailable = budget > allocated ? budget - allocated : 0
    return SystemInfo(
        ram_total_bytes: total,
        ram_available_bytes: available,
        vram_total_bytes: budget,
        vram_available_bytes: gpuAvailable,
        unified_memory: device?.hasUnifiedMemory ?? false,
        metal_available: device != nil,
        cpu_arch: architecture(),
        os_version: ProcessInfo.processInfo.operatingSystemVersionString,
        gpu_name: device?.name ?? ""
    )
}

do {
    guard CommandLine.arguments.count == 1 else {
        FileHandle.standardError.write(Data("Usage: fabrics-system-info\n".utf8))
        exit(EXIT_FAILURE)
    }
    if #available(macOS 10.15, *) {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.sortedKeys]
        var output = try encoder.encode(sample())
        output.append(0x0A)
        FileHandle.standardOutput.write(output)
    } else {
        FileHandle.standardError.write(Data("fabrics-system-info requires macOS 10.15 or newer\n".utf8))
        exit(EXIT_FAILURE)
    }
} catch {
    FileHandle.standardError.write(Data("fabrics-system-info: \(error)\n".utf8))
    exit(EXIT_FAILURE)
}
