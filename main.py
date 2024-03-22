import resource
import subprocess
import sys
import threading
import time
import psutil


def adjust_memory_limit(process, value):
    # Get resource usage limits
    soft, hard = resource.getrlimit(resource.RLIMIT_AS)

    # Set new soft limit
    resource.setrlimit(resource.RLIMIT_AS, (value, hard))


def adjust_cpu_limit(process, value):
    p = psutil.Process(process.pid)

    # Get the number of available logical CPUs
    num_cpus = psutil.cpu_count(logical=True)

    # Calculate the number of CPU cores to dedicate to the process
    num_cores = max(1, int(num_cpus * value / 100))

    # Set CPU affinity to limit the process to a specific number of cores
    p.cpu_affinity(list(range(num_cores)))

    # Set CPU priority to "below normal" to ensure other processes get fair CPU share
    p.nice(10)


def adjust_gpu_limit(process, value):
    # TODO
    pass


def get_max_memory():
    return 1024 * 1024 * 1024


def get_max_cpu():
    return 50


def get_max_gpu():
    return 50


def monitor_memory_and_cpu(process, process_finished):
    while not process_finished.is_set():
        max_memory_bytes = get_max_memory()
        max_cpu_percent = get_max_cpu()
        max_gpu_percent = get_max_gpu()

        adjust_memory_limit(process, max_memory_bytes)
        adjust_cpu_limit(process, max_cpu_percent)
        adjust_gpu_limit(process, max_gpu_percent)

        time.sleep(1)  # Adjust the sleep duration as needed


def run():
    # Run external program
    proc = subprocess.Popen(sys.argv[1:])

    # Flag to indicate whether the subprocess has finished
    process_finished = threading.Event()

    # Start monitoring memory and CPU
    monitor_thread = threading.Thread(target=monitor_memory_and_cpu,
                                      args=(proc, process_finished))
    monitor_thread.daemon = True
    monitor_thread.start()

    # Wait for the program to finish
    proc.wait()

    # Set the flag to indicate that the subprocess has finished
    process_finished.set()


if __name__ == '__main__':
    run()
