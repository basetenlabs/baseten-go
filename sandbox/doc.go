// Package sandbox creates and works with Baseten sandboxes.
//
// Create one [Client] up front and reuse it, then create a sandbox and run a
// command in it:
//
//	client, err := sandbox.NewClient(sandbox.ClientOptions{
//		APIKey: os.Getenv("BASETEN_API_KEY"),
//	})
//	if err != nil {
//		return err
//	}
//	sb, err := client.Create(ctx, sandbox.CreateOptions{})
//	if err != nil {
//		return err
//	}
//	process, err := sb.Process().Exec(ctx, sandbox.ProcessExecOptions{
//		Command:           "echo hello",
//		WaitForCompletion: true,
//	})
//	if err != nil {
//		return err
//	}
//	fmt.Print(process.Stdout)
package sandbox
