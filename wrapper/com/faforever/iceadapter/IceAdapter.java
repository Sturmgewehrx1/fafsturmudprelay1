package com.faforever.iceadapter;

import java.io.File;
import java.net.URISyntaxException;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;

/**
 * Drop-in replacement for the original faf-ice-adapter.jar.
 * Launches the Go relay-adapter binary with all CLI arguments passed through.
 * The Go binary must be in the same directory as this JAR.
 */
public class IceAdapter {

    public static void main(String[] args) throws Exception {
        File jarDir = getJarDirectory();

        String binaryName = System.getProperty("os.name").toLowerCase().contains("win")
                ? "faf-sturm-relay-adapter.exe"
                : "faf-sturm-relay-adapter";
        File binary = new File(jarDir, binaryName);

        if (!binary.exists()) {
            System.err.println("ERROR: Could not find relay adapter binary: " + binary.getAbsolutePath());
            System.err.println("The binary must be in the same directory as this JAR file.");
            System.exit(1);
        }

        List<String> command = new ArrayList<>();
        command.add(binary.getAbsolutePath());
        command.addAll(Arrays.asList(args));

        System.out.println("Starting relay adapter: " + String.join(" ", command));

        ProcessBuilder pb = new ProcessBuilder(command);
        pb.inheritIO();
        Process process = pb.start();

        Runtime.getRuntime().addShutdownHook(new Thread(() -> {
            if (process.isAlive()) {
                System.out.println("Shutting down relay adapter...");
                process.destroy();
                try {
                    process.waitFor();
                } catch (InterruptedException e) {
                    process.destroyForcibly();
                }
            }
        }));

        int exitCode = process.waitFor();
        System.exit(exitCode);
    }

    private static File getJarDirectory() {
        try {
            File jarFile = new File(
                    IceAdapter.class.getProtectionDomain()
                            .getCodeSource().getLocation().toURI());
            return jarFile.getParentFile();
        } catch (URISyntaxException e) {
            return new File(".");
        }
    }
}
