import java.io.File;
import java.net.URISyntaxException;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;

/**
 * Java wrapper for the faf-sturm-relay-adapter Go binary.
 * <p>
 * This JAR is a drop-in replacement for faf-ice-adapter.jar.
 * It starts the Go relay-adapter binary with all CLI arguments passed through.
 * The Go binary must be in the same directory as this JAR.
 * <p>
 * The relay server address is baked into the Go binary at build time.
 * No environment variables or config files needed.
 */
public class RelayAdapterWrapper {

    public static void main(String[] args) throws Exception {
        // Find the directory containing this JAR
        File jarDir = getJarDirectory();

        // Find the Go binary
        String binaryName = System.getProperty("os.name").toLowerCase().contains("win")
                ? "faf-sturm-relay-adapter.exe"
                : "faf-sturm-relay-adapter";
        File binary = new File(jarDir, binaryName);

        if (!binary.exists()) {
            System.err.println("ERROR: Could not find relay adapter binary: " + binary.getAbsolutePath());
            System.err.println("The binary must be in the same directory as this JAR file.");
            System.exit(1);
        }

        // Build command: binary + all CLI args passed through
        List<String> command = new ArrayList<>();
        command.add(binary.getAbsolutePath());
        command.addAll(Arrays.asList(args));

        System.out.println("Starting relay adapter: " + String.join(" ", command));

        // Start the process
        ProcessBuilder pb = new ProcessBuilder(command);
        pb.inheritIO();
        Process process = pb.start();

        // Shutdown hook to destroy the subprocess
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

        // Wait for the process to exit
        int exitCode = process.waitFor();
        System.exit(exitCode);
    }

    private static File getJarDirectory() {
        try {
            File jarFile = new File(
                    RelayAdapterWrapper.class.getProtectionDomain()
                            .getCodeSource().getLocation().toURI());
            return jarFile.getParentFile();
        } catch (URISyntaxException e) {
            // Fallback to current working directory
            return new File(".");
        }
    }
}
