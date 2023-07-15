# echo 3 > /proc/sys/vm/drop_caches

import psutil
import subprocess
import sys
import time
import os


def convert_bytes(num) -> str:
    for x in ["bytes", "KB", "MB", "GB", "TB"]:
        if num < 1024.0:
            return "%3.2f %s" % (num, x)
        num /= 1024.0


def monitor_process(proc, sample_rate):
    start_time = time.time()
    p = psutil.Process(proc.pid)

    cpu_use_list = []
    mem_use_list = []
    thread_count_list = []
    read_bytes_list = []
    write_bytes_list = []

    while proc.poll() is None:
        cpu = p.cpu_percent() / os.cpu_count()
        cpu_use_list.append(cpu)

        mem = p.memory_info().rss
        mem_use_list.append(mem)

        thread_count_list.append(p.num_threads())

        io_counters = p.io_counters()
        read_bytes_list.append(io_counters.read_bytes)
        write_bytes_list.append(io_counters.write_bytes)

        time.sleep(sample_rate)
    end_time = time.time()
    print("\n\n")

    print(f"Execution time: {end_time - start_time}")

    avg_cpu_use = sum(cpu_use_list) / len(cpu_use_list)
    avg_ram_use = convert_bytes(int(sum(mem_use_list) / len(mem_use_list)))
    avg_thread_count = int(sum(thread_count_list) / len(thread_count_list))
    avg_io_read = convert_bytes(int(sum(read_bytes_list) / len(read_bytes_list)))
    avg_io_write = convert_bytes(int(sum(write_bytes_list) / len(write_bytes_list)))

    print(f"Peak CPU Use: {max(cpu_use_list)}% | Avg CPU Use: {avg_cpu_use}%")
    print(
        f"Peak RAM Use: {convert_bytes(max(mem_use_list))} | Avg RAM Use: {avg_ram_use}"
    )
    print(f"Peak Threads: {max(thread_count_list)} | Avg Threads: {avg_thread_count}")
    print(
        f"Peak IO Read: {convert_bytes(max(read_bytes_list))} | Avg IO Read: {avg_io_read}"
    )
    print(
        f"Peak IO Write: {convert_bytes(max(write_bytes_list))} | Avg IO Write: {avg_io_write}"
    )


def main():
    if len(sys.argv) < 3:
        print("Please provide the program name as a command line argument")
        sys.exit(1)
    sample_rate = float(sys.argv[1])
    program_name = sys.argv[2]
    program_args = sys.argv[3:]
    proc = subprocess.Popen([program_name] + program_args)
    monitor_process(proc, sample_rate)
    print("-----------------------------------------------------------------------------\n\n")


if __name__ == "__main__":
    main()
