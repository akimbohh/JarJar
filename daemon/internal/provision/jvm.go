package provision

import "fmt"

// aikarFlags returns Aikar's well-known G1GC tuning flags for a Minecraft server
// with the given heap, in launch order (heap first, then the collector flags).
// These are the community-standard flags for a smooth, low-pause modded server;
// the region/new-size percentages shift for large (>=12 GB) heaps per Aikar's
// guidance. See https://docs.papermc.io/paper/aikars-flags.
func aikarFlags(memoryMB int) []string {
	flags := []string{
		fmt.Sprintf("-Xms%dM", memoryMB),
		fmt.Sprintf("-Xmx%dM", memoryMB),
		"-XX:+UseG1GC",
		"-XX:+ParallelRefProcEnabled",
		"-XX:MaxGCPauseMillis=200",
		"-XX:+UnlockExperimentalVMOptions",
		"-XX:+DisableExplicitGC",
		"-XX:+AlwaysPreTouch",
		"-XX:G1HeapWastePercent=5",
		"-XX:G1MixedGCCountTarget=4",
		"-XX:G1MixedGCLiveThresholdPercent=90",
		"-XX:G1RSetUpdatingPauseTimePercent=5",
		"-XX:SurvivorRatio=32",
		"-XX:+PerfDisableSharedMem",
		"-XX:MaxTenuringThreshold=1",
		"-Dusing.aikars.flags=https://mcflags.emc.gs",
		"-Daikars.new.flags=true",
	}
	// Heap-size-dependent region sizing.
	if memoryMB >= 12288 {
		flags = append(flags,
			"-XX:G1NewSizePercent=40",
			"-XX:G1MaxNewSizePercent=50",
			"-XX:G1HeapRegionSize=16M",
			"-XX:G1ReservePercent=15",
			"-XX:InitiatingHeapOccupancyPercent=20",
		)
	} else {
		flags = append(flags,
			"-XX:G1NewSizePercent=30",
			"-XX:G1MaxNewSizePercent=40",
			"-XX:G1HeapRegionSize=8M",
			"-XX:G1ReservePercent=20",
			"-XX:InitiatingHeapOccupancyPercent=15",
		)
	}
	return flags
}
