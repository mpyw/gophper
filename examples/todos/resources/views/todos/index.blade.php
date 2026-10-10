@extends('layout')

@section('title', 'Todos')

@section('content')
    <h1>Todos</h1>

    <form class="add" method="POST" action="{{ route('todos.store') }}">
        @csrf
        <input name="title" value="{{ old('title') }}" placeholder="What needs to be done?" autofocus>
        <button type="submit">Add</button>
    </form>
    @error('title')
        <p class="error">{{ $message }}</p>
    @enderror

    <ul>
        @forelse ($todos as $todo)
            <li @class(['done' => $todo->done])>
                <form method="POST" action="{{ route('todos.update', $todo) }}">
                    @csrf
                    @method('PATCH')
                    <button type="submit" title="Toggle">{{ $todo->done ? '☑' : '☐' }}</button>
                </form>
                <span class="title">{{ $todo->title }}</span>
                <form method="POST" action="{{ route('todos.destroy', $todo) }}">
                    @csrf
                    @method('DELETE')
                    <button type="submit" title="Delete">✕</button>
                </form>
            </li>
        @empty
            <li>Nothing to do.</li>
        @endforelse
    </ul>

    <p>{{ $todos->where('done', false)->count() }} left</p>
@endsection
